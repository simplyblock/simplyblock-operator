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
		"--hostnqn=nqn.2014-08.io.simplyblock:uuid:node-uid " +
		"--dhchap-secret=DHHC-1:00:secret: --dhchap-ctrl-secret=DHHC-1:00:ctrlsecret: --tls"

	got := dhchapAuthArgs(connect)
	want := []string{
		"--hostnqn=nqn.2014-08.io.simplyblock:uuid:node-uid",
		"--dhchap-secret=DHHC-1:00:secret:",
		"--dhchap-ctrl-secret=DHHC-1:00:ctrlsecret:",
		"--tls",
		"--hostid=node-uid",
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
