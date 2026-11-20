// The controller's way to the operator's placement service over its link.

package csilink

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/placement/placementrpc"
)

// A controller not linked to the operator answers Unavailable, which the
// provisioner retries, rather than an error that fails the claim.
func TestPlacementWithoutALinkIsUnavailable(t *testing.T) {
	agent, err := link.NewAgent(link.AgentConfig{
		Dial:  link.InsecureDialer("127.0.0.1:1"),
		ID:    link.ControllerPeer("csi-controller-0"),
		Token: link.StaticToken("token"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = Placement{Agent: agent}.Place(context.Background(), placementrpc.Request{})

	if status.Code(err) != codes.Unavailable {
		t.Errorf("Place without a link = %v, want Unavailable", err)
	}
}
