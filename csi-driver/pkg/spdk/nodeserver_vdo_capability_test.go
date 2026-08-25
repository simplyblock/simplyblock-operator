package spdk

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	csicommon "github.com/spdk/spdk-csi/pkg/csi-common"
)

// notVDOCapable is strconv.FormatBool(false) -- the label value an operator-managed,
// non-capable node carries, spelled out as a constant so it isn't a repeated string literal.
const notVDOCapable = "false"

func TestVDOCapableOperatorManaged(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
		want bool
	}{
		{
			name: "no label at all -- free for auto-detect to claim",
			node: &corev1.Node{},
			want: false,
		},
		{
			name: "label set by advertiseVDOCapability itself -- still auto-detect's to manage",
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Labels:      map[string]string{vdoCapableLabelKey: "true"},
				Annotations: map[string]string{vdoCapableManagedByAnnotationKey: vdoCapableManagedByAnnotationValue},
			}},
			want: false,
		},
		{
			name: "label set by hand, no managed-by annotation -- an operator override",
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{vdoCapableLabelKey: "true"},
			}},
			want: true,
		},
		{
			name: "label present with an unrelated annotation value -- still an override",
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Labels:      map[string]string{vdoCapableLabelKey: notVDOCapable},
				Annotations: map[string]string{vdoCapableManagedByAnnotationKey: "something-else"},
			}},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vdoCapableOperatorManaged(tt.node); got != tt.want {
				t.Errorf("vdoCapableOperatorManaged() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAdvertiseVDOCapability_RespectsOperatorOverride(t *testing.T) {
	const nodeName = "test-node"
	kubeClient := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			// An operator-set false, e.g., for an airgapped node the postStart hook's
			// dnf install could never reach -- no vdoCapableManagedByAnnotationKey.
			Labels: map[string]string{vdoCapableLabelKey: notVDOCapable},
		},
	})

	ns := &nodeServer{
		DefaultNodeServer: csicommon.NewDefaultNodeServer(csicommon.NewCSIDriver("test", "test", nodeName)),
		kubeClient:        kubeClient,
	}

	// The marker-file wait loop is unreachable once the operator-managed check fires, so
	// this call returns immediately without ever touching vdoCapableMarkerPath -- safe to
	// run for real rather than only exercising vdoCapableOperatorManaged in isolation.
	ns.advertiseVDOCapability(context.Background())

	updated, err := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node after advertiseVDOCapability: %v", err)
	}
	if got := updated.Labels[vdoCapableLabelKey]; got != notVDOCapable {
		t.Errorf("operator-set label was overwritten: got %q, want %q", got, notVDOCapable)
	}
	if _, ok := updated.Annotations[vdoCapableManagedByAnnotationKey]; ok {
		t.Errorf("operator-managed node unexpectedly gained the managed-by annotation")
	}
}
