// Tests the claim lease against the request it covers. The lease starts before
// the claim's patch and the call follows it, so a call that runs to the client's
// timeout ends after a lease no longer than that timeout. The next pass would
// then make the call again while the outcome of the first is still unknown, and
// Expand has no state of the cluster to skip on.

package cluster

import (
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

func TestTheClaimLeaseOutlivesARequestThatTimesOut(t *testing.T) {
	if claimLease <= webapi.RequestTimeout {
		t.Errorf("claimLease = %v, want it longer than the %v request timeout",
			claimLease, webapi.RequestTimeout)
	}
	if claimLease >= requestingDeadline {
		t.Errorf("claimLease = %v, want it inside the %v requestingDeadline, so a "+
			"crashed claim is retried before the step times out", claimLease, requestingDeadline)
	}
}
