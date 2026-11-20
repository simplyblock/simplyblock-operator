// The manual collection commands, run as written against a stub kubectl that
// serves the bundle-v10 fixture, must write a bundle this tool reads: the
// commands are the script's steps for someone who cannot run the script, and a
// step that writes the wrong file is a bundle missing what support asked for.

package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/support-tool/internal/bundletest"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

func TestTheManualCommandsWriteABundleTheToolReads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the commands are bash")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub, err := os.ReadFile(filepath.Join("testdata", "kubectl-stub.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), stub, 0o755); err != nil {
		t.Fatal(err)
	}

	steps := ManualCollection()
	if len(steps) < 5 {
		t.Fatalf("%d manual steps", len(steps))
	}
	var script strings.Builder
	script.WriteString("set -euo pipefail\n")
	for _, step := range steps {
		command := strings.ReplaceAll(step.Command, "<namespace>", "simplyblock")
		command = strings.ReplaceAll(command, "<run>", "discover-fleet-02")
		script.WriteString(command + "\n")
	}

	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", script.String())
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FIXTURE="+bundletest.Fixture(t, "bundle-v10"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the commands failed: %v\n%s\n--- script ---\n%s", err, out, script.String())
	}

	archives, _ := filepath.Glob(filepath.Join(work, "simplyblock-bundle-discover-fleet-02-*.tar.gz"))
	if len(archives) != 1 {
		t.Fatalf("the commands wrote %d archives", len(archives))
	}
	f, err := os.Open(archives[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, err := supportbundle.Read(f)
	if err != nil {
		t.Fatalf("the tool cannot read what the commands wrote: %v", err)
	}
	if b.Run == nil || b.Run.Name != "discover-fleet-02" || len(b.Reports) != 3 || len(b.Nodes) != 3 || b.CDC == nil {
		t.Errorf("the bundle holds run %v, %d reports, %d nodes, CDC %v", b.Run != nil, len(b.Reports), len(b.Nodes), b.CDC != nil)
	}
	for _, r := range b.Reports {
		if r.Err != "" {
			t.Errorf("report %s: %s", r.File, r.Err)
		}
	}
	if len(b.Problems) != 0 {
		t.Errorf("problems reading it: %v", b.Problems)
	}
}
