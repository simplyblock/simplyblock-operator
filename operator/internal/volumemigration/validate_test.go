package volumemigration

import (
	"slices"
	"strings"
	"testing"
)

func TestConnectArgs(t *testing.T) {
	base := Connection{NQN: "nqn.test:vol", IP: "10.0.0.1", Port: 4420, Transport: "tcp"}

	t.Run("required flags only when tuning is unset", func(t *testing.T) {
		got := connectArgs(base)
		want := []string{"connect", "-t", "tcp", "-a", "10.0.0.1", "-s", "4420", "-n", "nqn.test:vol"}
		if !slices.Equal(got, want) {
			t.Errorf("connectArgs = %v, want %v", got, want)
		}
	})

	t.Run("every tuning flag is passed when set", func(t *testing.T) {
		c := base
		c.NrIoQueues, c.ReconnectDelay, c.CtrlLossTmo, c.FastIOFailTmo, c.KeepAliveTmo = 3, 2, 3600, 8, 4
		got := strings.Join(connectArgs(c), " ")
		for _, want := range []string{
			"--nr-io-queues=3", "--reconnect-delay=2", "--ctrl-loss-tmo=3600",
			"--fast_io_fail_tmo=8", "--keep-alive-tmo=4",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("connectArgs = %q, want it to contain %q", got, want)
			}
		}
	})

	// A zero must mean "leave the kernel default", not "pass 0": nvme-cli rejects some
	// zero values outright, so passing them would fail the connect.
	t.Run("zero values are omitted, not passed as zero", func(t *testing.T) {
		got := strings.Join(connectArgs(base), " ")
		for _, absent := range []string{
			"--nr-io-queues", "--reconnect-delay", "--ctrl-loss-tmo",
			"--fast_io_fail_tmo", "--keep-alive-tmo",
		} {
			if strings.Contains(got, absent) {
				t.Errorf("connectArgs = %q, want no %s for a zero value", got, absent)
			}
		}
	})
}
