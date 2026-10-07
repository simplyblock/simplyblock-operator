package cluster

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCheckNameFits(t *testing.T) {
	// A real macOS home, since the budget depends on it.
	const state = "/Users/noctarius/.talos/clusters"

	tests := []struct {
		name    string
		cluster string
		wantErr bool
	}{
		{"four characters too long", "sb-integration-98062-cnc", true},
		{"the same name without the suffix", "sb-integration-98062", false},
		{"the short scheme, with a suffix", "sbi-98062-cnc", false},
		// 58 + 2N bytes with this state directory, against a 104-byte limit.
		{"the longest name that fits", strings.Repeat("x", 23), false},
		{"one character too many", strings.Repeat("x", 24), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkNameFits(state, tc.cluster)
			if tc.wantErr && err == nil {
				t.Fatalf("checkNameFits(%q) = nil, want a rejection", tc.cluster)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkNameFits(%q) = %v, want it accepted", tc.cluster, err)
			}
			// The message has to name the cause to be worth more than the
			// timeout it replaces.
			if err != nil && !strings.Contains(err.Error(), "twice") {
				t.Errorf("rejection does not explain why: %v", err)
			}
		})
	}
}

// A longer home directory eats the same budget, so the check cannot be a fixed
// name-length rule.
func TestCheckNameFits_AccountsForTheStateDirectory(t *testing.T) {
	const cluster = "sbi-98062-cnc"

	if err := checkNameFits("/Users/n/.talos/clusters", cluster); err != nil {
		t.Errorf("short home: %v", err)
	}
	long := "/Users/a-rather-long-user-name-on-a-long-path/.talos/clusters"
	if err := checkNameFits(long, cluster); err == nil {
		t.Errorf("checkNameFits(%q, %q) = nil; the state path leaves no room", long, cluster)
	}
}

// TestSurvivorMessageSaysWhatToDo. A create that fails leaves the cluster's
// QEMU processes running, and talosctl reports the teardown of a cluster whose
// state it could not read as a success and removes the state directory anyway.
// Nothing can find those processes afterward, and they hold a vmnet interface,
// so every later create on the host fails too, with an error about a port or an
// interface that says nothing about the cause. The teardown cannot kill them,
// since they belong to root, so what it owes is the exact command that can.
func TestSurvivorMessageSaysWhatToDo(t *testing.T) {
	msg := survivorMessage("sbi-4899", []string{"4914", "4916"})

	for _, want := range []string{"sbi-4899", "4914", "4916", "sudo kill -9"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not carry %q:\n%s", want, msg)
		}
	}
}

// TestSurvivorMessageIsEmptyWhenNothingSurvived keeps a clean teardown quiet.
func TestSurvivorMessageIsEmptyWhenNothingSurvived(t *testing.T) {
	if msg := survivorMessage("sbi-4899", nil); msg != "" {
		t.Errorf("a clean teardown reported: %s", msg)
	}
}

// TestWithTeardownCarriesBothFailures. A create that fails tears itself down,
// and the teardown can fail in its own way: talosctl cannot destroy a cluster
// that died before writing state.yaml, so the processes it started stay behind
// and only root can end them. Reporting the create's failure alone loses that,
// and the host is then unable to create another cluster for reasons the next
// run reports as a port conflict.
func TestWithTeardownCarriesBothFailures(t *testing.T) {
	create := errors.New("create cluster sbi-1: exit status 1")
	teardown := errors.New("cluster sbi-1 was not torn down: sudo kill -9 4914")

	err := withTeardown(create, teardown)

	for _, want := range []string{"exit status 1", "was not torn down", "4914"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q:\n%v", want, err)
		}
	}
	if !errors.Is(err, create) {
		t.Error("the create failure is no longer matchable, so a caller cannot classify it")
	}
}

// TestWithTeardownIsTheCreateFailureAlone when the teardown worked, which is
// the ordinary case and must stay unchanged.
func TestWithTeardownIsTheCreateFailureAlone(t *testing.T) {
	create := errors.New("create cluster sbi-1: exit status 1")
	if err := withTeardown(create, nil); err.Error() != create.Error() {
		t.Errorf("a clean teardown changed the error: %v", err)
	}
}

// TestLineWriterSplitsOnBothTerminators covers the reason this type exists.
// talosctl redraws its progress in place, so a carriage return ends a line as
// much as a newline does, and treating only newlines as terminators delivers a
// whole cluster creation as one line at the end.
func TestLineWriterSplitsOnBothTerminators(t *testing.T) {
	var got []string
	w := &lineWriter{emit: func(format string, args ...any) {
		got = append(got, args[0].(string))
	}}

	// Deliberately split mid-line across writes, because that is what a pipe
	// delivers and a writer that assumes whole lines loses the seam.
	for _, chunk := range []string{"downloading", " image\rbooting", " nodes\nwaiting"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	w.flush()

	want := []string{"downloading image", "booting nodes", "waiting"}
	if len(got) != len(want) {
		t.Fatalf("emitted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d is %q, want %q", i, got[i], want[i])
		}
	}
}

// TestLineWriterDropsEmptyLines keeps a redraw from emitting a blank line for
// every frame it paints.
func TestLineWriterDropsEmptyLines(t *testing.T) {
	emitted := 0
	w := &lineWriter{emit: func(string, ...any) { emitted++ }}

	if _, err := w.Write([]byte("\r\n   \r\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.flush()

	if emitted != 0 {
		t.Errorf("emitted %d lines from whitespace alone, want 0", emitted)
	}
}

// TestDefaultConfigNarrates pins the default this package chose. Silence is the
// wrong default for an operation measured in minutes, so a Config that names no
// logger still gets one.
func TestDefaultConfigNarrates(t *testing.T) {
	cfg := Config{}
	cfg.applyDefaults()

	if cfg.Logf == nil {
		t.Error("a defaulted Config discards talosctl's output, so a create is silent until it returns")
	}
}

func TestCreateArgs(t *testing.T) {
	base := Config{
		Name: "sbi-1", Controlplanes: 1, Workers: 1, CIDR: "10.5.0.0/24",
		ControlplaneMemoryMB: 2048, WorkerMemoryMB: 3072,
	}
	const factoryBoot = "cluster create qemu --name sbi-1 --controlplanes 1 --workers 1 --cidr 10.5.0.0/24 " +
		"--memory-controlplanes 2048mb --memory-workers 3072mb --presets disk-image " +
		"--config-patch @/w/patch.yaml --talosconfig-destination /w/talosconfig"

	t.Run("without a local image, the factory's is booted as before", func(t *testing.T) {
		if got := strings.Join(createArgs(base, "/w/patch.yaml", "/w/talosconfig", nil), " "); got != factoryBoot {
			t.Fatalf("createArgs =\n  %s\nwant\n  %s", got, factoryBoot)
		}
	})

	t.Run("a local image goes through the same preset, from the local factory", func(t *testing.T) {
		f := &localFactory{URL: "http://127.0.0.1:4711", Schematic: "local-0123456789abcdef"}
		got := strings.Join(createArgs(base, "/w/patch.yaml", "/w/talosconfig", f), " ")
		want := factoryBoot + " --image-factory-url http://127.0.0.1:4711 --schematic-id local-0123456789abcdef"
		if got != want {
			t.Fatalf("createArgs =\n  %s\nwant\n  %s", got, want)
		}
	})
}

// TestModulesPatch checks the extra modules land in the one module list the
// machine config reads, not in a second list a later key would replace.
func TestModulesPatch(t *testing.T) {
	var patch struct {
		Machine struct {
			Kernel struct {
				Modules []struct {
					Name string `yaml:"name"`
				} `yaml:"modules"`
			} `yaml:"kernel"`
		} `yaml:"machine"`
	}
	doc := nvmetPatch + extraModulesPatch([]string{"nfsd"}) + schedulablePatch
	if err := yaml.Unmarshal([]byte(doc), &patch); err != nil {
		t.Fatalf("the patch is not YAML: %v\n%s", err, doc)
	}
	var names []string
	for _, m := range patch.Machine.Kernel.Modules {
		names = append(names, m.Name)
	}
	if got := strings.Join(names, ","); got != "nvmet,nvmet_tcp,nfsd" {
		t.Fatalf("modules = %s, want nvmet,nvmet_tcp,nfsd\n%s", got, doc)
	}
	if extraModulesPatch(nil) != "" {
		t.Fatal("no extra modules must add nothing to the patch")
	}
}
