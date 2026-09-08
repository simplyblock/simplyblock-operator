// Verifies the first placement of a logical volume: which PVC annotation the CSI
// controller resolves into the host_id it sends on the CreateVolume POST, and in
// what priority. It lives beside the other controllerserver tests because it
// asserts on the wire body the driver produces (recorded by mockSBCLI), not on
// the intermediate helper — the annotation reaching the control plane is the
// behavior that decides whether a pinned volume is created in the right place or
// has to be migrated there afterward.
package spdk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/kube"
)

const (
	placementPVCName      = "fio-vol-worker-a"
	placementPVCNamespace = "fio-soak"

	// placementPinnedNode is the storage node a PVC pins its volume to, and
	// placementOtherNode is any different node, used to prove which annotation
	// wins when two of them name a target.
	placementPinnedNode = "6c538e1e-bd74-40ad-8c95-7f852c32cc2f"
	placementOtherNode  = "bc88b4a6-14f7-4f70-8a96-1eb855d3a98c"
	// placementColocatedNode is the storage node the pod's scheduled worker
	// hosts, offered through CSI topology rather than an annotation.
	placementColocatedNode = "9da282c7-fb74-444e-afda-dccffdb55fcb"
)

// placementPVC builds the PVC the controller reads its annotations from.
func placementPVC(annotations map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        placementPVCName,
			Namespace:   placementPVCNamespace,
			Annotations: annotations,
		},
	}
}

// colocationTopology is the accessibility requirement an external-provisioner
// with strict topology sends for a pod scheduled onto a worker that hosts
// placementColocatedNode, i.e., what Tier 1 co-location placement reads.
func colocationTopology() *csi.TopologyRequirement {
	return &csi.TopologyRequirement{
		Preferred: []*csi.Topology{{Segments: map[string]string{
			topologyKeyStorageNodeUUIDPrefix + sanityClusterID + ".0": placementColocatedNode,
		}}},
	}
}

// sentHostID drives one CreateVolume against the mock control plane and returns
// the host_id field of the create POST it produced.
func sentHostID(t *testing.T, mock *mockSBCLI, req *csi.CreateVolumeRequest, pvc *corev1.PersistentVolumeClaim) string {
	t.Helper()

	cs := newTestControllerServer(t, mock)
	cs.kubeClient = fake.NewSimpleClientset(pvc)

	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if len(mock.createVolumeBodies) != 1 {
		t.Fatalf("expected exactly 1 create POST, got %d", len(mock.createVolumeBodies))
	}

	var body struct {
		HostID string `json:"host_id"`
	}
	if err := json.Unmarshal(mock.createVolumeBodies[0], &body); err != nil {
		t.Fatalf("unmarshal create body %s: %v", mock.createVolumeBodies[0], err)
	}
	return body.HostID
}

// placementCreateVolumeRequest is basicCreateVolumeRequest plus the PVC identity
// parameters external-provisioner passes only when --extra-create-metadata is
// enabled. Without them the controller has no PVC to read, which is its own case
// below.
func placementCreateVolumeRequest(name string) *csi.CreateVolumeRequest {
	req := basicCreateVolumeRequest(name)
	req.Parameters["pool_name"] = sanityPoolUUID // UUID → no pool lookup
	req.Parameters[CSIStorageNameKey] = placementPVCName
	req.Parameters[CSIStorageNamespaceKey] = placementPVCNamespace
	return req
}

// TestCreateVolume_FirstPlacementHostID asserts the host_id the controller sends
// for every combination of placement annotations that can name a target, and for
// the two ways a target can go missing.
func TestCreateVolume_FirstPlacementHostID(t *testing.T) {
	tests := []struct {
		name string
		// annotations on the PVC the controller reads.
		annotations map[string]string
		// topology offered by the provisioner, nil for none.
		topology *csi.TopologyRequirement
		// omitPVCParams drops the csi.storage.k8s.io/pvc/{name,namespace}
		// parameters, modeling a provisioner without --extra-create-metadata.
		omitPVCParams bool
		wantHostID    string
	}{
		{
			name:        "selected-storage-node is sent as host_id",
			annotations: map[string]string{kube.AnnoSelectedStorageNode: placementPinnedNode},
			wantHostID:  placementPinnedNode,
		},
		{
			name: "selected-storage-node outranks placement-hint",
			annotations: map[string]string{
				kube.AnnoSelectedStorageNode: placementPinnedNode,
				kube.AnnoPlacementHint:       placementOtherNode,
			},
			wantHostID: placementPinnedNode,
		},
		{
			name: "selected-storage-node outranks the legacy host-id",
			annotations: map[string]string{
				kube.AnnoSelectedStorageNode: placementPinnedNode,
				kube.AnnoHostID:              placementOtherNode,
				kube.DeprecatedAnnoHostID:    placementOtherNode,
			},
			wantHostID: placementPinnedNode,
		},
		{
			name: "selected-storage-node outranks pod-affinity co-location",
			annotations: map[string]string{
				kube.AnnoSelectedStorageNode: placementPinnedNode,
				annotationPodAffinity:        "true",
			},
			topology:   colocationTopology(),
			wantHostID: placementPinnedNode,
		},
		{
			name:        "placement-hint is sent when there is no pin",
			annotations: map[string]string{kube.AnnoPlacementHint: placementOtherNode},
			wantHostID:  placementOtherNode,
		},
		{
			name:        "legacy host-id is sent when nothing else names a target",
			annotations: map[string]string{kube.AnnoHostID: placementOtherNode},
			wantHostID:  placementOtherNode,
		},
		{
			name: "co-location fills in only when no annotation names a target",
			annotations: map[string]string{
				annotationPodAffinity: "true",
			},
			topology:   colocationTopology(),
			wantHostID: placementColocatedNode,
		},
		{
			name:        "no annotation and no opt-in leaves placement to the control plane",
			annotations: nil,
			topology:    colocationTopology(),
			wantHostID:  "",
		},
		{
			name:          "a pin is not sent when the provisioner omits the PVC identity",
			annotations:   map[string]string{kube.AnnoSelectedStorageNode: placementPinnedNode},
			omitPVCParams: true,
			wantHostID:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockSBCLI()
			defer mock.Close()

			req := placementCreateVolumeRequest("pvc-placement")
			if tc.omitPVCParams {
				delete(req.Parameters, CSIStorageNameKey)
				delete(req.Parameters, CSIStorageNamespaceKey)
			}
			req.AccessibilityRequirements = tc.topology

			if got := sentHostID(t, mock, req, placementPVC(tc.annotations)); got != tc.wantHostID {
				t.Errorf("host_id on create POST = %q, want %q", got, tc.wantHostID)
			}
		})
	}
}

// TestCreateVolume_PinSurvivesProvisioning asserts the pin is left on the PVC
// after a successful create. Only the one-shot placement-hint is cleared: the pin
// has to stay for the operator's pin controller, drain, and rebalancer to keep
// honoring it for the life of the volume.
func TestCreateVolume_PinSurvivesProvisioning(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()

	pvc := placementPVC(map[string]string{
		kube.AnnoSelectedStorageNode: placementPinnedNode,
		kube.AnnoPlacementHint:       placementOtherNode,
	})
	cs := newTestControllerServer(t, mock)
	kubeClient := fake.NewSimpleClientset(pvc)
	cs.kubeClient = kubeClient

	if _, err := cs.CreateVolume(context.Background(), placementCreateVolumeRequest("pvc-pin-survives")); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	got, err := kubeClient.CoreV1().
		PersistentVolumeClaims(placementPVCNamespace).
		Get(context.Background(), placementPVCName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	if v := got.Annotations[kube.AnnoSelectedStorageNode]; v != placementPinnedNode {
		t.Errorf("%s = %q after provisioning, want it preserved as %q",
			kube.AnnoSelectedStorageNode, v, placementPinnedNode)
	}
	if v, ok := got.Annotations[kube.AnnoPlacementHint]; ok {
		t.Errorf("%s = %q after provisioning, want it cleared", kube.AnnoPlacementHint, v)
	}
}
