package volumemigration

import (
	"slices"
	"strings"
	"testing"
)

func TestParseAddress(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		wantIP   string
		wantPort int
	}{
		{"traddr and trsvcid", "traddr=10.0.0.1,trsvcid=4420", "10.0.0.1", 4420},
		{"reordered", "trsvcid=4420,traddr=10.0.0.1", "10.0.0.1", 4420},
		{"with src_addr, as the kernel reports it", "traddr=10.0.0.1,trsvcid=4420,src_addr=10.0.0.2", "10.0.0.1", 4420},
		{"spaces around separators", " traddr=10.0.0.1 , trsvcid=4420 ", "10.0.0.1", 4420},
		// Malformed input must degrade to a zero value, not panic: the caller compares
		// the result against expected paths, and a zero simply fails to match.
		{"empty", "", "", 0},
		{"no key/value pairs", "garbage", "", 0},
		{"non-numeric port", "traddr=10.0.0.1,trsvcid=abc", "10.0.0.1", 0},
		{"missing port", "traddr=10.0.0.1", "10.0.0.1", 0},
		{"missing address", "trsvcid=4420", "", 4420},
		{"value containing =", "traddr=10.0.0.1,trsvcid=4420,extra=a=b", "10.0.0.1", 4420},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip, port := parseAddress(tc.addr)
			if ip != tc.wantIP || port != tc.wantPort {
				t.Errorf("parseAddress(%q) = (%q, %d), want (%q, %d)",
					tc.addr, ip, port, tc.wantIP, tc.wantPort)
			}
		})
	}
}

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

// nvmeList builds `nvme list -v -ojson` output with one subsystem holding one
// controller at addr, whose paths have the given ANA states.
func nvmeList(nqn, addr string, anaStates ...string) string {
	paths := make([]string, 0, len(anaStates))
	for _, s := range anaStates {
		paths = append(paths, `{"ANAState":"`+s+`"}`)
	}
	return `{"Devices":[{"Subsystems":[{"SubsystemNQN":"` + nqn + `","Controllers":[` +
		`{"Address":"` + addr + `","Paths":[` + strings.Join(paths, ",") + `]}]}]}]}`
}

func TestCheckPaths(t *testing.T) {
	conn := Connection{NQN: "nqn.test:vol", IP: "10.0.0.1", Port: 4420, Transport: "tcp"}
	addr := "traddr=10.0.0.1,trsvcid=4420,src_addr=10.0.0.9"

	t.Run("inaccessible path present", func(t *testing.T) {
		if err := checkPaths([]byte(nvmeList(conn.NQN, addr, "inaccessible")), []Connection{conn}); err != nil {
			t.Errorf("checkPaths: %v", err)
		}
	})

	t.Run("one inaccessible among several states", func(t *testing.T) {
		if err := checkPaths([]byte(nvmeList(conn.NQN, addr, "optimized", "inaccessible")), []Connection{conn}); err != nil {
			t.Errorf("checkPaths: %v", err)
		}
	})

	// The negatives all mean "do not cut over": the new path is not in the state that
	// proves it was established but is not yet serving.
	t.Run("no inaccessible path", func(t *testing.T) {
		err := checkPaths([]byte(nvmeList(conn.NQN, addr, "optimized")), []Connection{conn})
		if err == nil {
			t.Fatalf("expected an error when no path is inaccessible")
		}
		if !strings.Contains(err.Error(), conn.NQN) {
			t.Errorf("error %q should name the missing path", err)
		}
	})

	t.Run("subsystem present at a different address", func(t *testing.T) {
		other := "traddr=10.9.9.9,trsvcid=4420"
		if err := checkPaths([]byte(nvmeList(conn.NQN, other, "inaccessible")), []Connection{conn}); err == nil {
			t.Errorf("expected an error: the inaccessible path is on another target address")
		}
	})

	t.Run("different subsystem at the right address", func(t *testing.T) {
		if err := checkPaths([]byte(nvmeList("nqn.other:vol", addr, "inaccessible")), []Connection{conn}); err == nil {
			t.Errorf("expected an error: the path belongs to another subsystem")
		}
	})

	t.Run("no controllers at all", func(t *testing.T) {
		if err := checkPaths([]byte(`{"Devices":[]}`), []Connection{conn}); err == nil {
			t.Errorf("expected an error when nvme reports no devices")
		}
	})

	t.Run("every expected path must be found", func(t *testing.T) {
		second := Connection{NQN: conn.NQN, IP: "10.0.0.2", Port: 4420, Transport: "tcp"}
		err := checkPaths([]byte(nvmeList(conn.NQN, addr, "inaccessible")), []Connection{conn, second})
		if err == nil {
			t.Fatalf("expected an error when only one of two paths is present")
		}
		if !strings.Contains(err.Error(), "10.0.0.2") {
			t.Errorf("error %q should name the missing second path", err)
		}
	})

	t.Run("malformed nvme output", func(t *testing.T) {
		if err := checkPaths([]byte("not json"), []Connection{conn}); err == nil {
			t.Errorf("expected a parse error")
		}
	})

	// No expectations means nothing to prove; used when a migration reports no
	// connect strings at all.
	t.Run("no expected connections", func(t *testing.T) {
		if err := checkPaths([]byte(`{"Devices":[]}`), nil); err != nil {
			t.Errorf("checkPaths with no expectations: %v", err)
		}
	})
}
