package reconnect

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/simplyblock/csi-driver/internal/controlplane"
	"github.com/simplyblock/csi-driver/internal/initiator"
)

// TestNodeHostNQNComputesAndCaches verifies NodeHostNQN derives the
// simplyblock-format host NQN from the Node's own UID, and that it's the
// same value on a second call even from a client that would now error (i.e.
// it's actually cached, not recomputed every time).
func TestNodeHostNQNComputesAndCaches(t *testing.T) {
	resetNodeHostNQNCache(t)

	const nodeName = "node-under-test"
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, UID: "node-uid-1234"},
	})

	got := NodeHostNQN(context.Background(), client, nodeName)
	want := "nqn.2014-08.io.simplyblock:uuid:node-uid-1234"
	if got != want {
		t.Fatalf("NodeHostNQN = %q, want %q", got, want)
	}

	// A client that would now error on any call proves the second call used
	// the cache instead of hitting the API again.
	erroringClient := fake.NewSimpleClientset()
	erroringClient.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("client should not be called again")
	})
	if got := NodeHostNQN(context.Background(), erroringClient, nodeName); got != want {
		t.Errorf("second NodeHostNQN call = %q, want cached %q", got, want)
	}
}

// TestNodeHostNQNRetriesAfterFailure verifies a failed lookup is not cached,
// so a later call with a working client still succeeds.
func TestNodeHostNQNRetriesAfterFailure(t *testing.T) {
	resetNodeHostNQNCache(t)

	const nodeName = "node-under-test"
	if got := NodeHostNQN(context.Background(), fake.NewSimpleClientset(), nodeName); got != "" {
		t.Fatalf("NodeHostNQN with no such node = %q, want empty", got)
	}

	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, UID: "node-uid-5678"},
	})
	const want = "nqn.2014-08.io.simplyblock:uuid:node-uid-5678"
	if got := NodeHostNQN(context.Background(), client, nodeName); got != want {
		t.Errorf("NodeHostNQN after a working client = %q, want %q", got, want)
	}
}

// resetNodeHostNQNCache clears NodeHostNQN's process-lifetime cache so tests
// don't leak state into each other.
func resetNodeHostNQNCache(t *testing.T) {
	t.Helper()
	nodeHostNQNMu.Lock()
	nodeHostNQNVal = ""
	nodeHostNQNMu.Unlock()
	t.Cleanup(func() {
		nodeHostNQNMu.Lock()
		nodeHostNQNVal = ""
		nodeHostNQNMu.Unlock()
	})
}

// ---- endpoint matching ----
func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name, address, wantIP, wantPort string
	}{
		{"traddr and trsvcid", "traddr=10.0.0.112,trsvcid=4428", "10.0.0.112", "4428"},
		{"with src_addr", "traddr=10.0.0.113,trsvcid=4430,src_addr=10.0.0.113", "10.0.0.113", "4430"},
		{"order does not matter", "trsvcid=4426,traddr=10.0.0.114", "10.0.0.114", "4426"},
		{"port missing", "traddr=10.0.0.112", "10.0.0.112", ""},
		{"empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, port := parseEndpoint(tt.address)
			if ip != tt.wantIP || port != tt.wantPort {
				t.Errorf("parseEndpoint(%q) = (%q, %q), want (%q, %q)",
					tt.address, ip, port, tt.wantIP, tt.wantPort)
			}
		})
	}
}

// The bug this fixes: a storage node serves one subsystem on several ports, so a stale
// controller at a port the control plane no longer publishes used to read as "this node is
// connected" and mask the endpoint it does publish. Matching on address alone left the
// volume a path short with a reconcile that found nothing to do every tick.
func TestMissingEndpoints_StaleControllerOnSameNodeDoesNotMaskAPublishedPort(t *testing.T) {
	conns := []*controlplane.LvolConnectResp{{IP: "10.0.0.112", Port: 4428}}
	active := []initiator.Path{{Address: "traddr=10.0.0.112,trsvcid=4426", State: "connecting"}}

	missing := missingEndpoints(conns, active)
	if len(missing) != 1 || missing[0].Port != 4428 {
		t.Fatalf("missingEndpoints = %+v, want the published 10.0.0.112:4428 reported missing", missing)
	}
}

// The same endpoint attached is the normal case and must not be connected again: a second
// connect to one endpoint adds a duplicate controller rather than replacing anything.
func TestMissingEndpoints_AttachedEndpointIsNotReconnected(t *testing.T) {
	conns := []*controlplane.LvolConnectResp{{IP: "10.0.0.112", Port: 4428}}
	active := []initiator.Path{{Address: "traddr=10.0.0.112,trsvcid=4428,src_addr=10.0.0.113", State: "live"}}

	if missing := missingEndpoints(conns, active); len(missing) != 0 {
		t.Errorf("missingEndpoints = %+v, want nothing for an already attached endpoint", missing)
	}
}

// A controller that is attached but cannot serve still counts as attached: the remedy is a
// teardown by the repairer, not a second controller for the same endpoint.
func TestMissingEndpoints_UnusableControllerStillCountsAsAttached(t *testing.T) {
	conns := []*controlplane.LvolConnectResp{{IP: "10.0.0.113", Port: 4430}}
	for _, p := range []initiator.Path{
		{Address: "traddr=10.0.0.113,trsvcid=4430", State: "connecting"},
		{Address: "traddr=10.0.0.113,trsvcid=4430", State: "live", ANAState: ""},
	} {
		if missing := missingEndpoints(conns, []initiator.Path{p}); len(missing) != 0 {
			t.Errorf("missingEndpoints with %+v = %+v, want it treated as attached", p, missing)
		}
	}
}

// An attached endpoint the control plane no longer publishes is neither reported as
// missing nor allowed to satisfy a published one. It is simply not consulted.
func TestMissingEndpoints_UnpublishedEndpointIsIgnored(t *testing.T) {
	conns := []*controlplane.LvolConnectResp{{IP: "10.0.0.112", Port: 4428}}
	active := []initiator.Path{
		{Address: "traddr=10.0.0.114,trsvcid=4430", State: "live", ANAState: "non-optimized"},
		{Address: "traddr=10.0.0.112,trsvcid=4428", State: "live", ANAState: "non-optimized"},
	}
	if missing := missingEndpoints(conns, active); len(missing) != 0 {
		t.Errorf("missingEndpoints = %+v, want the unpublished path ignored, not acted on", missing)
	}
}

// Several secondaries on one node, which is what the IP keying collapsed into one.
func TestMissingEndpoints_DistinctPortsOnOneNodeAreDistinctEndpoints(t *testing.T) {
	conns := []*controlplane.LvolConnectResp{
		{IP: "10.0.0.112", Port: 4426},
		{IP: "10.0.0.112", Port: 4428},
	}
	active := []initiator.Path{{Address: "traddr=10.0.0.112,trsvcid=4426", State: "live", ANAState: "non-optimized"}}

	missing := missingEndpoints(conns, active)
	if len(missing) != 1 || missing[0].Port != 4428 {
		t.Fatalf("missingEndpoints = %+v, want only :4428 missing", missing)
	}
}

func TestMissingEndpoints_NoActivePathsReportsAll(t *testing.T) {
	conns := []*controlplane.LvolConnectResp{{IP: "10.0.0.112", Port: 4428}, {IP: "10.0.0.114", Port: 4430}}
	if missing := missingEndpoints(conns, nil); len(missing) != 2 {
		t.Errorf("missingEndpoints = %+v, want both reported missing", missing)
	}
}
