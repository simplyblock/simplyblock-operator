package initiator

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestExecWithTimeoutPositive(t *testing.T) {
	elapsed, err := runExecWithTimeout([]string{"true"}, 10)
	if err != nil {
		t.Fatal("should succeed")
	}
	if elapsed > 3 {
		t.Fatal("timeout error")
	}
}

func TestExecWithTimeoutNegative(t *testing.T) {
	elapsed, err := runExecWithTimeout([]string{"false"}, 10)
	if err == nil {
		t.Fatal("should fail")
	}
	if elapsed > 3 {
		t.Fatal("timeout error")
	}
}

func TestExecWithTimeoutTimeout(t *testing.T) {
	elapsed, err := runExecWithTimeout([]string{"sleep", "10"}, 1)
	if err == nil {
		t.Fatal("should fail")
	}
	if elapsed > 3 {
		t.Fatal("timeout error")
	}
}

func runExecWithTimeout(cmdLine []string, timeout int) (int, error) {
	start := time.Now()
	err := execWithTimeout(context.Background(), cmdLine, timeout)
	elapsed := int(time.Since(start) / time.Second)
	return elapsed, err
}

// TestDHCHAPAuthArgsExtractsHostIdentityAndSecrets verifies that the host
// identity and DHCHAP/TLS flags are pulled out of the control-plane-supplied
// connect command line, that a --hostid matching the hostnqn's own UUID is
// synthesized (so it can never collide with the node's file-based default
// hostid, which is paired with a different hostnqn), and that unrelated
// flags (already covered by connectViaNVMe's own args) are ignored.
func TestDHCHAPAuthArgsExtractsHostIdentityAndSecrets(t *testing.T) {
	connect := "sudo nvme connect --reconnect-delay=2 --ctrl-loss-tmo=3600 " +
		"--transport=tcp --traddr=1.2.3.4 --trsvcid=4420 --nqn=nqn.test " +
		"--hostnqn=nqn.2014-08.io.simplyblock:uuid:2f8c4b1e-9d3a-4c77-b210-5e6f7a8b9c0d " +
		"--dhchap-secret=DHHC-1:00:secret: --dhchap-ctrl-secret=DHHC-1:00:ctrlsecret: --tls"

	got := dhchapAuthArgs(connect)
	want := []string{
		"--hostnqn=nqn.2014-08.io.simplyblock:uuid:2f8c4b1e-9d3a-4c77-b210-5e6f7a8b9c0d",
		"--dhchap-secret=DHHC-1:00:secret:",
		"--dhchap-ctrl-secret=DHHC-1:00:ctrlsecret:",
		"--tls",
		"--hostid=2f8c4b1e-9d3a-4c77-b210-5e6f7a8b9c0d",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("dhchapAuthArgs(%q) = %v, want %v", connect, got, want)
	}
}

// TestDHCHAPAuthArgsNoAuthWhenUnneeded verifies that a connect command with no
// host identity or secrets (the common case for a pool without allowed_hosts)
// yields no extra flags.
func TestDHCHAPAuthArgsNoAuthWhenUnneeded(t *testing.T) {
	connect := "sudo nvme connect --transport=tcp --traddr=1.2.3.4 --trsvcid=4420 --nqn=nqn.test"
	if got := dhchapAuthArgs(connect); len(got) != 0 {
		t.Errorf("dhchapAuthArgs(%q) = %v, want empty", connect, got)
	}
}

// TestDHCHAPAuthArgsNoHostIDWithoutAUUID pins what adopting atlas nqn.HostUUID
// tightened. The hand-rolled predecessor took whatever followed the last colon
// of the host NQN and passed it as --hostid; nqn.HostUUID requires the :uuid:
// marker and a well-formed UUID, so a host NQN in any other shape now yields
// the --hostnqn alone.
//
// No caller can reach the difference: this driver's only host NQN comes from
// nqn.Host(node.UID), and a Kubernetes node UID is a UUID. The tightening is
// kept rather than worked around because the kernel compares hostid as a UUID,
// so a --hostid that is not one could only ever have failed the connect.
func TestDHCHAPAuthArgsNoHostIDWithoutAUUID(t *testing.T) {
	for _, hostNQN := range []string{
		"nqn.2014-08.io.simplyblock:uuid:not-a-uuid",
		"nqn.2014-08.io.simplyblock:node-1",
		"nqn.2014-08.org.nvmexpress:uuid:",
	} {
		got := dhchapAuthArgs("nvme connect --hostnqn=" + hostNQN)
		want := []string{"--hostnqn=" + hostNQN}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("dhchapAuthArgs for %q = %v, want %v", hostNQN, got, want)
		}
	}
}
