// Tests for the SAN initiator reading: what a worker reports to be registered
// on a storage array as, read from a fake host root and sysfs.

package inventory

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadSANInitiators(t *testing.T) {
	host, sys := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(host, "etc/iscsi/initiatorname.iscsi"),
		"## DO NOT EDIT\n#InitiatorName=iqn.commented\nInitiatorName=iqn.1994-05.com.redhat:a1b2\n")
	writeFile(t, filepath.Join(host, "etc/nvme/hostnqn"), "nqn.2014-08.org.nvmexpress:uuid:5a6b\n")
	writeFile(t, filepath.Join(sys, "class/fc_host/host3/port_name"), "0x21000024FF4C1A2B\n")
	writeFile(t, filepath.Join(sys, "class/fc_host/host4/port_name"), "0x0000000000000000\n")
	writeFile(t, filepath.Join(sys, "class/fc_host/host5/port_name"), "0x21000024ff4c1a2c\n")

	got, err := ReadSANInitiators(Config{HostRoot: host, SysfsRoot: sys})
	if err != nil {
		t.Fatal(err)
	}
	if got.ISCSIIQN != "iqn.1994-05.com.redhat:a1b2" {
		t.Errorf("IQN = %q", got.ISCSIIQN)
	}
	if got.NVMeHostNQN != "nqn.2014-08.org.nvmexpress:uuid:5a6b" {
		t.Errorf("host NQN = %q", got.NVMeHostNQN)
	}
	// An unconfigured port (all zeroes) is not an initiator.
	if !slices.Equal(got.FCWWPNs, []string{"21000024ff4c1a2b", "21000024ff4c1a2c"}) {
		t.Errorf("WWPNs = %v", got.FCWWPNs)
	}
}

// A host with no SAN software reports nothing, and that is not an error.
func TestReadSANInitiatorsOnABareHost(t *testing.T) {
	got, err := ReadSANInitiators(Config{HostRoot: t.TempDir(), SysfsRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got.ISCSIIQN != "" || got.NVMeHostNQN != "" || len(got.FCWWPNs) != 0 {
		t.Errorf("a bare host reported %+v", got)
	}
}
