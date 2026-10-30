// The mover's refusal to aim a move at a node of a stretched cluster's other
// site, kept apart from the interface properties in mover_test.go because it
// holds only for the kind that resolves a backend UUID to a StorageNode.

package volumemigration

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// TestAMoveToTheOtherSitesNodeIsRefused. On a stretched cluster each site lists
// the other site's nodes as remote StorageNode records, so a peer-site backend
// UUID resolved to one of them and the mover created a migration onto a node
// this site does not manage.
//
// Regression: 2026-10-22-remote-node-migration-target.
func TestAMoveToTheOtherSitesNodeIsRefused(t *testing.T) {
	ctx := context.Background()
	world := moveWorld()
	world[1] = &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: moveNodeName, Namespace: testNamespace},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			Config:    simplyblockv1alpha2.StorageNodeConfig{Site: "site-b"},
			Stretched: &simplyblockv1alpha2.StretchedNodeSpec{Remote: true},
		},
		Status: simplyblockv1alpha2.StorageNodeStatus{UUID: moveTargetID},
	}
	c := fake.NewClientBuilder().WithScheme(moverScheme(t)).WithObjects(world...).Build()
	mover := &OperationMover{Client: c}

	err := mover.Start(ctx, moveRequest())
	if err == nil {
		t.Fatal("a move onto the other site's node was accepted")
	}
	if !strings.Contains(err.Error(), "peer site") {
		t.Errorf("the refusal reads %q, want it to say the node is on the peer site", err)
	}

	var operations simplyblockv1alpha2.PersistentVolumeOpsList
	if err := c.List(ctx, &operations, client.InNamespace("")); err != nil {
		t.Fatal(err)
	}
	if len(operations.Items) != 0 {
		t.Errorf("found %d operations, want none aimed at the other site", len(operations.Items))
	}
}
