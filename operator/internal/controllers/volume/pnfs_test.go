// A pNFS volume is not moved by an operation.
//
// The handle on its PersistentVolume is the backing logical volume's own, so
// every lookup an operation makes succeeds and the control plane would accept
// the migration. What no step accounts for is the export in front of it: the
// metadata server and every client node hold paths to the namespace, and the
// validation and release Jobs only know a block consumer's.

package volume

import (
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestAPNFSVolumeFailsTheOperationWithoutAMigration(t *testing.T) {
	pv := testVolumeObject()
	pv.Spec.CSI.FSType = kube.FSTypePNFS
	api := idleSubsystem()
	r := testReconciler(t, api, testOperation(), pv, testClusterObject(), testNodeObject())

	for range 3 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if !strings.Contains(ops.Status.Message, "pNFS") {
		t.Errorf("message = %q, want it to say the volume is pNFS", ops.Status.Message)
	}
	if api.creates != 0 {
		t.Errorf("the create was sent %d times, want never", api.creates)
	}
}

// The positive half: a filesystem volume is moved as before.
func TestAFilesystemVolumeStillMigrates(t *testing.T) {
	pv := testVolumeObject()
	pv.Spec.CSI.FSType = "ext4"
	api := idleSubsystem()
	world := []client.Object{testOperation(), pv, testClusterObject(), testNodeObject()}
	r := testReconciler(t, api, world...)

	for range 3 {
		runPass(t, r)
	}

	if api.creates != 1 {
		t.Errorf("the create was sent %d times, want once", api.creates)
	}
}
