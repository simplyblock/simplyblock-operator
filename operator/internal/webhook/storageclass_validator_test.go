// Tests for the StorageClass admission webhook: which classes assigned to a
// pool are refused, and which pass because a class may differ from its pool.
package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/kube"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/pool"
)

const (
	scNamespace   = "simplyblock"
	scCluster     = "production"
	scClusterUUID = "4f2c8a11-6b3d-4e19-9a55-0c7e1d8f2b34"
	scPool        = "fast"
)

func newStorageClassValidator(t *testing.T, clusterUUID string) *StorageClassValidator {
	t.Helper()
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: scCluster, Namespace: scNamespace},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: clusterUUID},
	}
	striped := &simplyblockv1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: scPool, Namespace: scNamespace},
		Spec: simplyblockv1alpha2.StoragePoolSpec{
			ClusterRef: scCluster,
			VolumeDefaults: &simplyblockv1alpha2.VolumeDefaults{
				StripeWidth: ptr.To[int32](4),
				StripeSize:  "64Ki",
				Filesystem:  "xfs",
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(newPoolScheme(t)).WithObjects(cluster, striped).Build()
	return &StorageClassValidator{Client: cl}
}

// assigned is a class carrying the three labels that assign it to pool name.
func assigned(name, poolName string, params map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			pool.LabelNamespace: scNamespace,
			pool.LabelCluster:   scCluster,
			pool.LabelPool:      poolName,
		}},
		Provisioner: "csi.simplyblock.io",
		Parameters:  params,
	}
}

func classRaw(t *testing.T, class *storagev1.StorageClass) runtime.RawExtension {
	t.Helper()
	b, err := json.Marshal(class)
	if err != nil {
		t.Fatalf("marshal the class: %v", err)
	}
	return runtime.RawExtension{Raw: b}
}

func classRequest(t *testing.T, op admissionv1.Operation, class, old *storagev1.StorageClass) admission.Request {
	t.Helper()
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: op,
		Name:      class.Name,
		Object:    classRaw(t, class),
	}}
	if old != nil {
		req.OldObject = classRaw(t, old)
	}
	return req
}

// reaching is the parameters that reach the pool, plus extra.
func reaching(extra map[string]string) map[string]string {
	params := map[string]string{kube.ParamClusterID: scClusterUUID, kube.ParamPool: scPool}
	for k, v := range extra {
		params[k] = v
	}
	return params
}

// Regression: 2026-11-17-pool-stripe-never-reaches-class (PR #922 review). A
// class assigned to a striped pool that does not stripe provisioned ordinary
// volumes with nothing saying so; it is refused when it is written.
func TestStorageClassValidatorRefusesAClassThatDoesNotStripeAsItsPool(t *testing.T) {
	v := newStorageClassValidator(t, scClusterUUID)
	for name, params := range map[string]map[string]string{
		"no stripe":         reaching(nil),
		"another width":     reaching(map[string]string{kube.ParamStripeWidth: "2"}),
		"another size":      reaching(map[string]string{kube.ParamStripeWidth: "4", kube.ParamStripeSize: "128Ki"}),
		"an unreadable one": reaching(map[string]string{kube.ParamStripeWidth: "four"}),
	} {
		resp := v.Handle(context.Background(), classRequest(t, admissionv1.Create, assigned("c", scPool, params), nil))
		if resp.Allowed || !strings.Contains(resp.Result.Message, "stripe") {
			t.Errorf("%s: allowed=%v %q, want a refusal naming the stripe", name, resp.Allowed, resp.Result.Message)
		}
	}
}

// A class reaching another cluster or pool than its labels say provisions
// somewhere else, and is refused.
func TestStorageClassValidatorRefusesAClassThatProvisionsSomewhereElse(t *testing.T) {
	v := newStorageClassValidator(t, scClusterUUID)
	for name, params := range map[string]map[string]string{
		"another cluster": {kube.ParamClusterID: "another", kube.ParamPool: scPool, kube.ParamStripeWidth: "4"},
		"another pool":    {kube.ParamClusterID: scClusterUUID, kube.ParamPool: "slow", kube.ParamStripeWidth: "4"},
	} {
		resp := v.Handle(context.Background(), classRequest(t, admissionv1.Create, assigned("c", scPool, params), nil))
		if resp.Allowed {
			t.Errorf("%s: a class reaching somewhere else was admitted", name)
		}
	}
}

// What passes: a class stating the pool's stripe in another spelling, one that
// overrides an ordinary default, one assigned to no pool, and one assigned to a
// pool that does not exist yet.
func TestStorageClassValidatorAdmits(t *testing.T) {
	v := newStorageClassValidator(t, scClusterUUID)
	unassigned := assigned("plain", scPool, reaching(nil))
	unassigned.Labels = nil
	for name, class := range map[string]*storagev1.StorageClass{
		"the pool's stripe": assigned("c", scPool, reaching(map[string]string{
			kube.ParamStripeWidth: "4", kube.ParamStripeSize: "65536",
		})),
		"an overridden filesystem": assigned("c", scPool, reaching(map[string]string{
			kube.ParamStripeWidth: "4", "csi.storage.k8s.io/fstype": "ext4",
		})),
		"no assignment":        unassigned,
		"a pool not yet there": assigned("c", "later", reaching(nil)),
	} {
		resp := v.Handle(context.Background(), classRequest(t, admissionv1.Create, class, nil))
		if !resp.Allowed {
			t.Errorf("%s: refused: %q", name, resp.Result.Message)
		}
	}
}

// A cluster without a UUID yet cannot be compared with, and the stripe still is.
func TestStorageClassValidatorChecksTheStripeBeforeTheClusterHasAUUID(t *testing.T) {
	v := newStorageClassValidator(t, "")
	ok := assigned("c", scPool, map[string]string{kube.ParamClusterID: "whatever", kube.ParamPool: scPool, kube.ParamStripeWidth: "4"})
	if resp := v.Handle(context.Background(), classRequest(t, admissionv1.Create, ok, nil)); !resp.Allowed {
		t.Errorf("refused before the cluster had a UUID: %q", resp.Result.Message)
	}
	unstriped := assigned("c", scPool, map[string]string{kube.ParamClusterID: "whatever", kube.ParamPool: scPool})
	if resp := v.Handle(context.Background(), classRequest(t, admissionv1.Create, unstriped, nil)); resp.Allowed {
		t.Error("a class that does not stripe was admitted before the cluster had a UUID")
	}
}

// A class's parameters are immutable, so an update that leaves its assignment
// alone changes nothing this checks, and is admitted even for a class that
// predates the webhook. One that assigns the class to the pool is checked.
func TestStorageClassValidatorChecksAnUpdateOnlyWhenItAssigns(t *testing.T) {
	v := newStorageClassValidator(t, scClusterUUID)
	old := assigned("c", scPool, reaching(nil))
	relabeled := old.DeepCopy()
	relabeled.Labels["team"] = "data"
	if resp := v.Handle(context.Background(), classRequest(t, admissionv1.Update, relabeled, old)); !resp.Allowed {
		t.Errorf("an update leaving the assignment alone was refused: %q", resp.Result.Message)
	}

	unassigned := old.DeepCopy()
	unassigned.Labels = nil
	if resp := v.Handle(context.Background(), classRequest(t, admissionv1.Update, old, unassigned)); resp.Allowed {
		t.Error("an update assigning a class that does not stripe to a striped pool was admitted")
	}
}
