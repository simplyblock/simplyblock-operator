package cluster

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Regression (integration CI, 2026-11-25): ubuntu-latest moved to sudo-rs,
// which does not support -E. It warns on stderr and drops the environment, so
// talosctl ran with root's HOME (a fresh image download, state under
// /root/.talos) and without the KUBECONFIG that keeps its merge out of the
// caller's own file. The variables talosctl needs are passed by name instead.
func TestTalosctlCommand_SudoPassesHomeAndKubeconfigByName(t *testing.T) {
	t.Setenv("HOME", "/home/runner")
	cmd := talosctlCommand(context.Background(), "/usr/local/bin/talosctl", true,
		"/work/kubeconfig", []string{"config", "info"})

	want := []string{"sudo", "env", "HOME=/home/runner", "KUBECONFIG=/work/kubeconfig",
		"/usr/local/bin/talosctl", "config", "info"}
	if filepath.Base(cmd.Path) != "sudo" || !slices.Equal(cmd.Args, want) {
		t.Fatalf("command = %q, want %q", cmd.Args, want)
	}
	if slices.Contains(cmd.Args, "-E") {
		t.Errorf("command still relies on sudo -E: %q", cmd.Args)
	}
}

func TestTalosctlCommand_WithoutSudoSetsKubeconfigInTheEnvironment(t *testing.T) {
	cmd := talosctlCommand(context.Background(), "/usr/local/bin/talosctl", false,
		"/work/kubeconfig", []string{"config", "info"})

	if want := []string{"/usr/local/bin/talosctl", "config", "info"}; !slices.Equal(cmd.Args, want) {
		t.Fatalf("command = %q, want %q", cmd.Args, want)
	}
	if !slices.Contains(cmd.Env, "KUBECONFIG=/work/kubeconfig") {
		t.Errorf("environment does not name the cluster's kubeconfig")
	}
}

// Regression (integration CI, 2026-11-25): sudo's warning arrived in the same
// buffer as the JSON, and every suite failed with "parse talos context:
// invalid character 's'". Whatever lands on stderr is not part of the answer.
func TestControlplaneAddresses_IgnoresWhatArrivesOnStderr(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "talosctl")
	script := "#!/bin/sh\n" +
		"echo \"sudo: preserving the entire environment is not supported, '-E' is ignored\" >&2\n" +
		"echo '{\"context\":\"sbi-1\",\"nodes\":null,\"endpoints\":[\"10.5.0.2\"]}'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	noSudo := false
	c := &Cluster{cfg: Config{TalosctlPath: fake, Sudo: &noSudo},
		kubeconfig: filepath.Join(t.TempDir(), "kubeconfig"), talosconfig: "unused"}

	got, err := c.controlplaneAddresses(context.Background())
	if err != nil {
		t.Fatalf("controlplaneAddresses: %v", err)
	}
	if !slices.Equal(got, []string{"10.5.0.2"}) {
		t.Fatalf("addresses = %q, want [10.5.0.2]", got)
	}
}
