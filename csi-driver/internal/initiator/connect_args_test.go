// The NVMe connect command line per path: two-node arbitrated clusters get the
// control plane's keep-alive, fast_io_fail and ctrl-loss values, a cluster
// whose control plane shortened the keep-alive (multi-attach) gets that
// keep-alive, and every other cluster connects exactly as before.

package initiator

import (
	"slices"
	"strconv"
	"testing"

	"github.com/simplyblock/csi-driver/internal/controlplane"
)

// path is a connect entry as the control plane returns it: keep-alive 2 on a
// two-node cluster, 8 (sbcli LVOL_NVME_KEEP_ALIVE_TO_TCP) on a regular one.
func path(twoNode bool) *controlplane.LvolConnectResp {
	kato := regularKeepAliveTmo
	if twoNode {
		kato = 2
	}
	return &controlplane.LvolConnectResp{
		Nqn: "nqn.2023-02.io.simplyblock:c:lvol:v", ReconnectDelay: 2, NrIoQueues: 4,
		CtrlLossTmo: 3600, Port: 4420, TargetType: "TCP", IP: "10.0.0.1",
		Connect:      "sudo nvme connect --keep-alive-tmo=" + strconv.Itoa(kato),
		KeepAliveTmo: kato, FastIOFailTmo: 8, TwoNode: twoNode,
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

// pathWithKeepAlive is a path of a cluster without two-node arbitration whose
// control plane returned the given keep-alive.
func pathWithKeepAlive(kato int) *controlplane.LvolConnectResp {
	p := path(false)
	p.KeepAliveTmo = kato
	p.Connect = "sudo nvme connect --keep-alive-tmo=" + strconv.Itoa(kato)
	return p
}

// A regular cluster's control plane returns keep-alive 8 on every path, and an
// older one none: neither changes the command line.
func TestConnectArgsIgnoreARegularKeepAlive(t *testing.T) {
	want := connectArgs(path(false), DefaultCtrlLossTmo)
	for _, kato := range []int{0, regularKeepAliveTmo, 30} {
		if got := connectArgs(pathWithKeepAlive(kato), DefaultCtrlLossTmo); !slices.Equal(got, want) {
			t.Errorf("keep-alive %d: args = %v\nwant   %v", kato, got, want)
		}
	}
}

// A multi-attach cluster's control plane returns keep-alive 3: it is passed on,
// and only it -- fast_io_fail and ctrl-loss stay the driver's.
func TestConnectArgsCarryAShortenedKeepAlive(t *testing.T) {
	got := connectArgs(pathWithKeepAlive(3), DefaultCtrlLossTmo)
	want := []string{"nvme", "connect", "-t", "tcp", "-a", "10.0.0.1", "-s", "4420",
		"-n", "nqn.2023-02.io.simplyblock:c:lvol:v", "-l", "60", "-c", "2", "-i", "4", "-k", "3"}
	if !slices.Equal(got, want) {
		t.Errorf("args = %v\nwant   %v", got, want)
	}
}

// Two-node paths are unchanged: keep-alive and fast_io_fail once each.
func TestConnectArgsTwoNodeKeepAliveOnce(t *testing.T) {
	got := connectArgs(path(true), DefaultCtrlLossTmo)
	n := 0
	for _, a := range got {
		if a == "-k" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("args %v carry -k %d times, want 1", got, n)
	}
}

// The staged path (the volume stack) carries the same keep-alive: a shortened
// one is passed, a regular or two-node path keeps the kernel default as before.
func TestConnectionFromCarriesAShortenedKeepAlive(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp *controlplane.LvolConnectResp
		want int
	}{
		{"multi-attach", pathWithKeepAlive(3), 3},
		{"regular", pathWithKeepAlive(regularKeepAliveTmo), 0},
		{"older control plane", pathWithKeepAlive(0), 0},
		{"two-node", path(true), 0},
	} {
		connection, _ := ConnectionFrom([]*controlplane.LvolConnectResp{tc.resp}, "ns")
		if got := connection.Endpoints[0].KeepAliveTMOSec; got != tc.want {
			t.Errorf("%s: keep-alive = %d, want %d", tc.name, got, tc.want)
		}
	}
}
