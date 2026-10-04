// The pre-join live migration (docs/consistency-group-colocation.md §5, in
// sbcli): a volume whose PVC gets the consistency-group label while it lives
// off the group's pinned node/LVS is moved there first, then joined.
//
// The move is requested as a VolumeMigration, not made against the control
// plane directly: a live migration needs the consumer host attached to the
// target's paths before the backend moves the data, and attaching them is the
// operator's VolumeMigration controller's job. The CSI driver only asks for
// the move and reads how it went, through a dynamic client, so it does not
// depend on the operator's API types.
package controller

import (
	"context"
	"fmt"
	"os"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var volumeMigrationGVR = schema.GroupVersionResource{
	Group: "storage.simplyblock.io", Version: "v1alpha1", Resource: "volumemigrations",
}

// preJoinPurposeLabel marks the VolumeMigrations the watcher creates, so they
// are told apart from the rebalancer's and an operator's own.
const preJoinPurposeLabel = "storage.simplyblock.io/purpose"

// volumeMigrations creates VolumeMigrations in the driver's namespace.
type volumeMigrations struct {
	client    dynamic.Interface
	namespace string
}

func newVolumeMigrations(client dynamic.Interface) *volumeMigrations {
	ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	if ns == "" {
		ns = "simplyblock"
	}
	return &volumeMigrations{client: client, namespace: ns}
}

// Ensure creates the named VolumeMigration when it does not exist and returns
// its status.phase. An existing one is never rewritten: a request for another
// target (the group re-pinned meanwhile) waits until the old one is deleted.
func (m *volumeMigrations) Ensure(ctx context.Context, name, pvName, targetNodeUUID string) (string, error) {
	res := m.client.Resource(volumeMigrationGVR).Namespace(m.namespace)
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		obj = &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "storage.simplyblock.io/v1alpha1",
			"kind":       "VolumeMigration",
			"metadata": map[string]any{
				"name":      name,
				"namespace": m.namespace,
				"labels": map[string]any{
					preJoinPurposeLabel:            "consistency-group-join",
					"app.kubernetes.io/managed-by": "spdkcsi",
				},
			},
			"spec": map[string]any{
				"pvName":         pvName,
				"targetNodeUUID": targetNodeUUID,
			},
		}}
		created, cerr := res.Create(ctx, obj, metav1.CreateOptions{})
		if cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return "", fmt.Errorf("create VolumeMigration %s/%s: %w", m.namespace, name, cerr)
		}
		if cerr == nil {
			obj = created
		} else if obj, err = res.Get(ctx, name, metav1.GetOptions{}); err != nil {
			return "", fmt.Errorf("read VolumeMigration %s/%s: %w", m.namespace, name, err)
		}
	} else if err != nil {
		return "", fmt.Errorf("read VolumeMigration %s/%s: %w", m.namespace, name, err)
	}
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	return phase, nil
}

// watcherOptions reads the co-location switches from the environment:
// SPDKCSI_CG_PREJOIN_MIGRATION (default true), SPDKCSI_CG_COLOCATE (default
// false) and SPDKCSI_CG_CLIENT_SWAP_READY (default false).
func watcherOptions() (preJoin, colocate, swapReady bool) {
	flag := func(name string, def bool) bool {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		default:
			return def
		}
	}
	return flag("SPDKCSI_CG_PREJOIN_MIGRATION", true),
		flag("SPDKCSI_CG_COLOCATE", false),
		flag("SPDKCSI_CG_CLIENT_SWAP_READY", false)
}
