// What a discovery run reports about the draft it wrote beyond the draft
// itself: the generator's findings, such as topology only some workers carry,
// reach the reviewer as Warning events on the run under their own reasons.

package deployment

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	discoverypkg "github.com/simplyblock/simplyblock-operator/internal/discovery"
)

func TestAFleetWithSomeZonesReportsTheWorkersWithout(t *testing.T) {
	var fleet *discoveryCase
	for _, c := range loadDiscoveryCases(t) {
		if c.Name == "fleet/fleet-02-three-uniform-workers" {
			fleet = &c
		}
	}
	if fleet == nil {
		t.Fatal("the fleet-02 case is missing")
	}
	for _, object := range fleet.Objects {
		if node, ok := object.(*corev1.Node); ok && node.Name == "worker-01" {
			node.Labels = map[string]string{corev1.LabelTopologyZone: "zone-a"}
		}
	}

	scheme := opsScheme(t)
	ops := fleet.Ops.DeepCopy()
	recorder := events.NewFakeRecorder(1024)
	reconciler := &OperatorOpsReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(fleet.Objects, ops)...).
			WithStatusSubresource(ops).Build(),
		Scheme: scheme, Recorder: recorder, ProbeImage: opsImage,
	}
	if _, err := reconciler.write(context.Background(), ops); err != nil {
		t.Fatalf("write: %v", err)
	}

	var reported []string
	for len(recorder.Events) > 0 {
		if line := <-recorder.Events; strings.Contains(line, discoverypkg.TopologyIncomplete) {
			reported = append(reported, line)
		}
	}
	if len(reported) != 1 || !strings.HasPrefix(reported[0], corev1.EventTypeWarning) ||
		!strings.Contains(reported[0], "worker-02") || strings.Contains(reported[0], "worker-01") {
		t.Errorf("%s events = %v, want one Warning naming the unlabeled workers", discoverypkg.TopologyIncomplete, reported)
	}
}
