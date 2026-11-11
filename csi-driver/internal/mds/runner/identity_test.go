// Tests for the runner's two names: the guest's hostname is the StatefulSet's,
// shared by every pod, and the link peer is the pod's own.

package runner

import (
	"testing"

	"github.com/simplyblock/atlas/link"
)

func TestEachPodIsAPeerOfItsOwnWhateverTheGuestsHostname(t *testing.T) {
	id, err := PeerID("pnfs-mds-1", "pnfs-mds")
	if err != nil {
		t.Fatal(err)
	}
	if id != link.MDSPeer("pnfs-mds-1") {
		t.Errorf("peer = %v, want mds/pnfs-mds-1", id)
	}
}

func TestAnUnnamedPodOrGuestIsRefused(t *testing.T) {
	for _, c := range [][2]string{{"", "pnfs-mds"}, {"pnfs-mds-0", ""}} {
		if id, err := PeerID(c[0], c[1]); err == nil {
			t.Errorf("PeerID(%q, %q) = %v, want an error", c[0], c[1], id)
		}
	}
}
