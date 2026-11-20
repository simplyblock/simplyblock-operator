// The NVMe connect command line per path: two-node arbitrated clusters get the
// control plane's keep-alive, fast_io_fail and ctrl-loss values, every other
// cluster connects exactly as before.

package initiator

import (
	"slices"
	"testing"

	"github.com/simplyblock/csi-driver/internal/controlplane"
)

func path(twoNode bool) *controlplane.LvolConnectResp {
	return &controlplane.LvolConnectResp{
		Nqn: "nqn.2023-02.io.simplyblock:c:lvol:v", ReconnectDelay: 2, NrIoQueues: 4,
		CtrlLossTmo: 3600, Port: 4420, TargetType: "TCP", IP: "10.0.0.1",
		Connect:      "sudo nvme connect --keep-alive-tmo=2",
		KeepAliveTmo: 2, FastIOFailTmo: 8, TwoNode: twoNode,
	}
}

func TestConnectArgsUnchangedForRegularClusters(t *testing.T) {
	got := connectArgs(path(false), DefaultCtrlLossTmo)
	want := []string{"nvme", "connect", "-t", "tcp", "-a", "10.0.0.1", "-s", "4420",
		"-n", "nqn.2023-02.io.simplyblock:c:lvol:v", "-l", "60", "-c", "2", "-i", "4"}
	if !slices.Equal(got, want) {
		t.Errorf("args = %v\nwant   %v", got, want)
	}
}

// Keep-alive 2 s finds a frozen node's path; fast_io_fail 8 s answers held I/O
// before failing it; ctrl-loss 3600 s keeps retrying a fenced path.
func TestConnectArgsCarryTheTwoNodeSettings(t *testing.T) {
	got := connectArgs(path(true), DefaultCtrlLossTmo)
	for _, want := range [][]string{{"-l", "3600"}, {"-k", "2"}, {"--fast_io_fail_tmo=8"}} {
		if i := slices.Index(got, want[0]); i < 0 || (len(want) == 2 && got[i+1] != want[1]) {
			t.Errorf("args %v lack %v", got, want)
		}
	}
}

// A volume context that asks for a longer ctrl-loss than the control plane keeps it.
func TestConnectArgsKeepALongerCtrlLoss(t *testing.T) {
	got := connectArgs(path(true), 7200)
	if i := slices.Index(got, "-l"); got[i+1] != "7200" {
		t.Errorf("ctrl-loss = %s, want 7200", got[i+1])
	}
}
