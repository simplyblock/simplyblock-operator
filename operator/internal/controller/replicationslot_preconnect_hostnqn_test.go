package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/nqn"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// Bed bf29 (2026-11-19): the replication preconnect Jobs attached every path as
// the kernel's default host NQN while the CSI staged the volume as the node's
// simplyblock host NQN, so the cutover freeze guard counted one client as two
// hosts and held every cutover of round 2.

func TestPreconnectConnectsAsTheNodesHostNQN(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "client-1", UID: types.UID("5843f627-4d54-444d-949c-df1934456b0e")}}
	r, _ := newSlotReconciler(t, node)
	conns := []vmigration.Connection{{NQN: "nqn.2023-02.io.simplyblock:c:lvol:v", IP: "10.0.0.1", Port: 4420},
		{NQN: "nqn.2023-02.io.simplyblock:c:lvol:v", IP: "10.0.0.2", Port: 4420}}

	got := r.withNodeHostNQN(context.Background(), conns, "client-1")

	want := nqn.Host("5843f627-4d54-444d-949c-df1934456b0e")
	for i, c := range got {
		if c.HostNQN != want {
			t.Fatalf("connection %d host NQN = %q, want %q", i, c.HostNQN, want)
		}
	}
	if conns[0].HostNQN != "" {
		t.Fatalf("the caller's connections were modified")
	}
}

func TestPreconnectKeepsTheConnectionsWhenTheNodeCannotBeRead(t *testing.T) {
	r, _ := newSlotReconciler(t)
	conns := []vmigration.Connection{{NQN: "nqn.2023-02.io.simplyblock:c:lvol:v", IP: "10.0.0.1", Port: 4420}}

	got := r.withNodeHostNQN(context.Background(), conns, "missing-node")

	if len(got) != 1 || got[0].HostNQN != "" {
		t.Fatalf("got %+v, want the connections unchanged", got)
	}
}
