// How many journal managers a node is added with.
//
// The requirement is the control plane's and it has more than one trigger: four
// where the cluster can lose two chunks, four where failure domains are enabled
// at any parity level, three otherwise. It also computes that for itself when the
// request carries no ha_jm_count, which is what makes not sending one the whole
// of the operator's part.

package node

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// TestAnUnstatedJournalCountIsLeftToTheControlPlane is the defect.
//
// Regression: 2026-09-21-ha-jm-count-3-on-a-two-parity-cluster — three was sent
// whenever a node stated no count. That is the control plane's answer for a
// cluster that can lose one chunk with failure domains off, and it was sent for
// every cluster: a 2+2 deployment failed every node add, saying that an
// ha_jm_count of 3 was too low for a max_fault_tolerance of 2 and that the
// minimum required was 4. It retried on a loop,
// and held the cluster's only node-add slot while it did, so the other five
// workers queued behind a worker that could never finish.
func TestAnUnstatedJournalCountIsLeftToTheControlPlane(t *testing.T) {
	if got := journalCount(nil); got != 0 {
		t.Errorf("a node stating no journal count is added with ha_jm_count=%d, "+
			"which is this side answering a question the control plane answers", got)
	}

	empty := &simplyblockv1alpha2.JournalManagerSpec{}
	if got := journalCount(empty); got != 0 {
		t.Errorf("an empty journal block is added with ha_jm_count=%d", got)
	}
}

// Zero has to leave the request rather than travel as zero, because zero is not
// a count the control plane would accept either.
func TestAnUnstatedJournalCountIsNotOnTheWire(t *testing.T) {
	body, err := json.Marshal(utils.StorageNodeSetAddParams{
		HaJMCount: journalCount(nil),
	})
	if err != nil {
		t.Fatalf("marshal the add: %v", err)
	}
	if strings.Contains(string(body), "ha_jm_count") {
		t.Errorf("the add carries ha_jm_count with nothing to say: %s", body)
	}
}

// A stated count is the deployment's and is sent as it stands. The control plane
// refuses one below its floor, which is the right place for that to be decided.
func TestAStatedJournalCountIsSent(t *testing.T) {
	spec := &simplyblockv1alpha2.JournalManagerSpec{Count: ptr.To(int32(6))}

	if got := journalCount(spec); got != 6 {
		t.Errorf("a stated count of 6 was sent as %d", got)
	}
}
