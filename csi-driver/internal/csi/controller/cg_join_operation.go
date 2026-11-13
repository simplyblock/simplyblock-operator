// The join operation a late consistency-group label turns into
// (design-cg-live-subsystem-flip.md §8.1): one PersistentVolumeOps with action
// JoinConsistencyGroup per volume and group, which the operator runs end to end
// (leaving the volume's old group when it is relabeled, the migration to the
// group's node, the join, and the move into the group's subsystem under a live
// consumer).
//
// The CSI driver only asks for the operation and reads how it went, through a
// dynamic client, so it does not depend on the operator's API types.
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

var persistentVolumeOpsGVR = schema.GroupVersionResource{
	Group: "storage.simplyblock.io", Version: "v1alpha2", Resource: "persistentvolumeops",
}

// joinOperations creates JoinConsistencyGroup operations. PersistentVolumeOps
// is cluster-scoped, because the PersistentVolume it acts on is.
type joinOperations struct {
	client dynamic.Interface
}

func newJoinOperations(client dynamic.Interface) *joinOperations {
	return &joinOperations{client: client}
}

// Ensure creates the named operation when it does not exist and returns its
// status.phase, and whether this call created it. An existing one is never
// rewritten or recreated: a failed operation stays until it is deleted, and
// deleting it is how a retry is asked for.
func (j *joinOperations) Ensure(ctx context.Context, name, pvName, groupName string) (string, bool, error) {
	res := j.client.Resource(persistentVolumeOpsGVR)
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	created := false
	switch {
	case apierrors.IsNotFound(err):
		obj = &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "storage.simplyblock.io/v1alpha2",
			"kind":       "PersistentVolumeOps",
			"metadata": map[string]any{
				"name": name,
				"labels": map[string]any{
					preJoinPurposeLabel:            "consistency-group-join",
					"app.kubernetes.io/managed-by": "spdkcsi",
				},
			},
			"spec": map[string]any{
				"persistentVolumeName": pvName,
				"action":               "JoinConsistencyGroup",
				"joinConsistencyGroup": map[string]any{"groupName": groupName},
			},
		}}
		made, cerr := res.Create(ctx, obj, metav1.CreateOptions{})
		switch {
		case cerr == nil:
			obj, created = made, true
		case apierrors.IsAlreadyExists(cerr):
			if obj, err = res.Get(ctx, name, metav1.GetOptions{}); err != nil {
				return "", false, fmt.Errorf("read PersistentVolumeOps %s: %w", name, err)
			}
		default:
			return "", false, fmt.Errorf("create PersistentVolumeOps %s: %w", name, cerr)
		}
	case err != nil:
		return "", false, fmt.Errorf("read PersistentVolumeOps %s: %w", name, err)
	}
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	return phase, created, nil
}

// joinOperationEnabled reads SPDKCSI_CG_JOIN_OPERATION, on unless it says off.
// Off, the watcher joins directly and requests the pre-join VolumeMigration as
// it did before the operation existed.
func joinOperationEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SPDKCSI_CG_JOIN_OPERATION"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// The PersistentVolumeOps phases the watcher reports (operator api/v1alpha2).
const (
	opsSucceeded = "Succeeded"
	opsFailed    = "Failed"
	opsAborted   = "Aborted"
)
