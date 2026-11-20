// The nvme-tcp-module init container's guard script, run for real: under sh,
// against a fake host root, with chroot replaced by a stub that records how it
// was called. What it proves is the branch the guard takes on each host: only
// an Ubuntu host is entered, and every other host exits 0 so that the node
// plugin still starts, where the plugin's own check reports a missing module.

package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runNVMeTCPGuard runs the guard with osRelease as the host's /etc/os-release,
// or with none when osRelease is empty, and returns its output and the
// arguments the chroot stub received, empty when it was not called.
func runNVMeTCPGuard(t *testing.T, osRelease string) (output, chrootArgs string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	host := t.TempDir()
	if osRelease != "" {
		if err := os.MkdirAll(filepath.Join(host, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(host, "etc", "os-release"), []byte(osRelease), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "chroot-args")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\n"
	if err := os.WriteFile(filepath.Join(bin, "chroot"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	c := nvmeTCPModuleContainer("img", "IfNotPresent")
	cmd := exec.Command(c.Command[0], c.Command[1:]...)
	cmd.Env = append(os.Environ(),
		"HOST_ROOT="+host,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("guard exited with %v: %s", err, out)
	}
	args, _ := os.ReadFile(record)
	return string(out), string(args)
}

func TestNVMeTCPGuardEntersAnUbuntuHost(t *testing.T) {
	_, args := runNVMeTCPGuard(t, "NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"22.04\"\n")
	lines := strings.Split(strings.TrimSpace(args), "\n")
	if len(lines) < 3 || lines[1] != "/bin/sh" || lines[2] != "-c" {
		t.Fatalf("chroot args = %q, want <host> /bin/sh -c <script>", lines)
	}
	if !strings.Contains(args, "linux-modules-extra-$(uname -r)") ||
		!strings.Contains(args, "DPkg::Lock::Timeout=120") ||
		!strings.Contains(args, "/etc/modules-load.d/nvme-tcp.conf") {
		t.Errorf("the script run inside the host is not the Ubuntu install: %s", args)
	}
}

func TestNVMeTCPGuardLeavesOtherHostsAlone(t *testing.T) {
	for name, osRelease := range map[string]string{
		"rhel":          "NAME=\"Red Hat Enterprise Linux CoreOS\"\nID=\"rhcos\"\n",
		"talos":         "NAME=\"Talos\"\nID=talos\n",
		"no os-release": "",
	} {
		t.Run(name, func(t *testing.T) {
			out, args := runNVMeTCPGuard(t, osRelease)
			if args != "" {
				t.Errorf("chroot was called with %q on a non-Ubuntu host", args)
			}
			if !strings.Contains(out, "nvme-tcp") {
				t.Errorf("output %q says nothing about nvme-tcp", out)
			}
		})
	}
}
