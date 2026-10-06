// The identity a discovery run's probes write their reports as.
//
// The operator creates it with the run, in the run's namespace, and owns it from
// the run. A chart-rendered account sat in one namespace chosen at install time,
// and a run raised anywhere else started probe pods that had nothing to run as.
// The Role grants two verbs on one kind because that is the whole of what a probe
// does to the cluster: it writes one report ConfigMap, and a retried Job
// replaces it. Reading the reports and cleaning them up is the operator's, and a
// probe that could delete them could delete another worker's.

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// probeIdentityPrefix names the three objects, which share one name.
const (
	probeIdentityPrefix = "sb-nodeprobe-"

	// maxIdentityNameLength is what a ServiceAccount's name may be, which is the
	// tightest of the three kinds' limits.
	maxIdentityNameLength = 253
)

// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create
// rbac-justified: the probe Role grants configmaps create and update, which the manager already holds.

// probeIdentityName is the one name the run's account, Role, and binding take. A
// run name too long to prefix is shortened with a hash of itself, so two long
// names still differ.
func probeIdentityName(run string) string {
	name := probeIdentityPrefix + run
	if len(name) <= maxIdentityNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(run))
	suffix := hex.EncodeToString(sum[:])[:10]
	return name[:maxIdentityNameLength-len(suffix)-1] + "-" + suffix
}

// ensureProbeIdentity creates the run's account, Role, and binding where they are
// absent and returns the account's name. An object that exists is left as it is,
// which is what lets the probe step run again on every requeue.
func (r *OperatorOpsReconciler) ensureProbeIdentity(
	ctx context.Context, namespace, run string, owner *metav1.OwnerReference,
) (string, error) {
	name := probeIdentityName(run)
	meta := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{*owner},
			Labels: map[string]string{
				nodeprobe.LabelComponent: nodeprobe.ComponentNodeProbe,
				nodeprobe.LabelRun:       run,
			},
		}
	}

	identity := []client.Object{
		&corev1.ServiceAccount{ObjectMeta: meta()},
		&rbacv1.Role{
			ObjectMeta: meta(),
			// One report per worker per run, created once and replaced if the Job
			// retried its pod. No get, list, or delete.
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"create", "update"},
			}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: meta(),
			Subjects: []rbacv1.Subject{{
				Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: namespace,
			}},
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		},
	}
	for _, obj := range identity {
		if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create %T %s/%s: %w", obj, namespace, name, err)
		}
	}
	return name, nil
}
