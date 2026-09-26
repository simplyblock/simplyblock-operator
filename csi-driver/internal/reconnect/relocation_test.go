// whitebox test of what the recheck concludes when a volume stops being where it was
package reconnect

import (
	"context"
	"testing"

	"github.com/simplyblock/csi-driver/internal/initiator"
)

// An lvol migrated onto a node pair this host never connected to disappears from
// every path the host holds, all at once: the ANA flip cannot steer to an address
// with no controller behind it, and deleting the source subsystems takes the rest.
// The host then has one ctrl_loss_tmo to be pointed at the new location.
//
// Found live 2026-09-24: a volume migrated during a node removal, the recheck
// reported the subsystem gone, recovery was skipped as a spurious ANA switchover,
// and 63 seconds later the kernel removed both controllers and ext4 aborted its
// journal under a running workload.

const relocatedNQN = "nqn.2023-02.io.simplyblock:cluster:lvol:bc12e3fe-229f-4f7d-9bb4-c08411d62c66"

// stubSubsystems stands in for the host's NVMe topology for one test.
func stubSubsystems(t *testing.T, answer []initiator.SubsystemResponse) {
	t.Helper()
	original := subsystemsForDevice
	subsystemsForDevice = func(context.Context, string) ([]initiator.SubsystemResponse, error) {
		return answer, nil
	}
	t.Cleanup(func() { subsystemsForDevice = original })
}

func oneSubsystem(nqn string, paths int) []initiator.SubsystemResponse {
	p := make([]initiator.Path, paths)
	for i := range p {
		p[i] = initiator.Path{Name: "nvme0", State: "live", ANAState: "optimized"}
	}
	return []initiator.SubsystemResponse{{Subsystems: []initiator.Subsystem{{NQN: nqn, Paths: p}}}}
}

// The regression: a subsystem gone from every path must ask for recovery, because
// recovery is the only thing that re-resolves where the volume went.
func TestAVanishedSubsystemAsksForRecovery(t *testing.T) {
	stubSubsystems(t, []initiator.SubsystemResponse{{Subsystems: []initiator.Subsystem{}}})

	sub := &initiator.Subsystem{NQN: relocatedNQN, Paths: make([]initiator.Path, 2)}
	if !confirmSubsystemNeedsRecovery(context.Background(), sub, "/dev/nvme1n1", 2) {
		t.Error("a volume that disappeared from every path was treated as needing no " +
			"recovery, so nothing re-resolves it and the host fails its I/O when " +
			"ctrl_loss_tmo expires")
	}
}

// A subsystem still present with the same number of paths is the steady state the
// debounce was written for: nothing to do.
func TestAStableSubsystemConfirms(t *testing.T) {
	stubSubsystems(t, oneSubsystem(relocatedNQN, 3))

	sub := &initiator.Subsystem{NQN: relocatedNQN, Paths: make([]initiator.Path, 3)}
	if !confirmSubsystemNeedsRecovery(context.Background(), sub, "/dev/nvme1n1", 3) {
		t.Error("a subsystem holding steady at its path count did not confirm")
	}
}

// A path count that moves under the recheck is the spurious ANA switchover the
// debounce exists to swallow, and it still swallows it.
func TestAMovingPathCountIsStillDebounced(t *testing.T) {
	stubSubsystems(t, oneSubsystem(relocatedNQN, 3))

	sub := &initiator.Subsystem{NQN: relocatedNQN, Paths: make([]initiator.Path, 2)}
	if confirmSubsystemNeedsRecovery(context.Background(), sub, "/dev/nvme1n1", 2) {
		t.Error("a path count that changed during the recheck was treated as settled, " +
			"which is the flapping the debounce is there to absorb")
	}
}

// Another volume's subsystem on the same device says nothing about this one.
func TestAnotherVolumesSubsystemDoesNotCountAsThisOne(t *testing.T) {
	stubSubsystems(t, oneSubsystem("nqn.2023-02.io.simplyblock:cluster:lvol:something-else", 3))

	sub := &initiator.Subsystem{NQN: relocatedNQN, Paths: make([]initiator.Path, 2)}
	if !confirmSubsystemNeedsRecovery(context.Background(), sub, "/dev/nvme1n1", 2) {
		t.Error("a different volume's subsystem was read as this volume still being present")
	}
}
