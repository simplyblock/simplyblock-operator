// What the probe accepts on its command line, and the one flag it refuses to
// default.
//
// The interesting case is --mountinfo. Every other root has a sensible default,
// and this one has a default that is wrong in a pod and wrong in the direction
// that hands over a mounted disk, so a report destined for the cluster is
// refused without it. The rest of the cases below are here because the Job
// builds this command line and nothing else does: a flag renamed on one side
// and not the other is a failure nobody sees until a probe pod exits non-zero.

package main

import (
	"strings"
	"testing"
)

// noEnv is the environment of a probe run by hand, with none of what the Job
// sets.
func noEnv(string) string { return "" }

// envOf serves a fixed environment, which is how the Job's field references
// reach the process.
func envOf(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestParseOptionsRefusesAClusterReportWithNoMountTable(t *testing.T) {
	// Unset, the disk reading consults this process's own mount table, which in
	// a pod lists none of the host's mounts: every mounted host disk would be
	// reported free. There is no safe default, because only the caller knows
	// where it mounted the host's /proc.
	_, err := parseOptions([]string{
		"--node=worker-3", "--namespace=simplyblock", "--run=oops-1",
		"--sysfs-root=/host/sys", "--proc-root=/host/proc",
	}, noEnv)

	if err == nil {
		t.Fatal("accepted a run that writes a report without being told which mount table to read")
	}
	if !strings.Contains(err.Error(), "--mountinfo") {
		t.Errorf("the error is %q, and it does not name the flag that is missing", err)
	}
}

func TestParseOptionsAcceptsAClusterReportWithOne(t *testing.T) {
	opts, err := parseOptions([]string{
		"--node=worker-3", "--namespace=simplyblock", "--run=oops-1",
		"--mountinfo=/host/proc/1/mountinfo",
	}, noEnv)
	if err != nil {
		t.Fatalf("parse the Job's own flags: %v", err)
	}
	if opts.roots.MountinfoPath != "/host/proc/1/mountinfo" {
		t.Errorf("read the mount table %q", opts.roots.MountinfoPath)
	}
	if opts.output != outputConfigMap {
		t.Errorf("the default output is %q, want %q", opts.output, outputConfigMap)
	}
}

func TestParseOptionsLeavesTheMountTableOptionalForAHandRun(t *testing.T) {
	// A person running the probe on a machine to see what it would report is
	// not in a pod, so their own mount table is the host's and requiring the
	// flag would be requiring them to write down the default.
	opts, err := parseOptions([]string{"--node=worker-3", "--output=stdout"}, noEnv)
	if err != nil {
		t.Fatalf("parse a hand run: %v", err)
	}
	if opts.roots.MountinfoPath != "" {
		t.Errorf("defaulted the mount table to %q for a hand run", opts.roots.MountinfoPath)
	}
}

func TestParseOptionsTakesTheAmbientFactsFromTheEnvironment(t *testing.T) {
	// The Job passes the node and the namespace as field references rather than
	// literals, so the probe reads them from the environment and needs no
	// access to the API to learn where it landed.
	opts, err := parseOptions([]string{
		"--run=oops-1", "--mountinfo=/host/proc/1/mountinfo",
	}, envOf(map[string]string{
		"NODE_NAME":     "worker-7",
		"POD_NAMESPACE": "simplyblock",
	}))
	if err != nil {
		t.Fatalf("parse with the Job's environment: %v", err)
	}
	if opts.node != "worker-7" || opts.namespace != "simplyblock" {
		t.Errorf("read node %q in namespace %q", opts.node, opts.namespace)
	}
}

func TestParseOptionsReadsTheOwnerTheJobPassed(t *testing.T) {
	// All four parts or none: a partial reference is one the API server
	// rejects, and losing a worker's whole inventory to that would be worse
	// than an uncollected report.
	full := envOf(map[string]string{
		"OWNER_API_VERSION": "storage.simplyblock.io/v1alpha1",
		"OWNER_KIND":        "OperatorOps",
		"OWNER_NAME":        "oops-1",
		"OWNER_UID":         "8f14e45f-ceea-467a-9d1f-2e0b1c4b6b8a",
	})
	opts, err := parseOptions([]string{"--node=n", "--namespace=ns", "--run=r", "--mountinfo=/m"}, full)
	if err != nil {
		t.Fatalf("parse with an owner: %v", err)
	}
	if opts.owner == nil || opts.owner.Name != "oops-1" {
		t.Fatalf("read the owner %+v", opts.owner)
	}

	partial := envOf(map[string]string{"OWNER_KIND": "OperatorOps", "OWNER_NAME": "oops-1"})
	opts, err = parseOptions([]string{"--node=n", "--namespace=ns", "--run=r", "--mountinfo=/m"}, partial)
	if err != nil {
		t.Fatalf("parse with a partial owner: %v", err)
	}
	if opts.owner != nil {
		t.Errorf("built the owner %+v out of half a reference", opts.owner)
	}
}

func TestParseOptionsRefusesWhatItCannotRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no node at all", []string{"--namespace=ns", "--run=r", "--mountinfo=/m"}},
		{"no namespace to write into", []string{"--node=n", "--run=r", "--mountinfo=/m"}},
		{"no run to attribute it to", []string{"--node=n", "--namespace=ns", "--mountinfo=/m"}},
		{"an output nobody has", []string{"--node=n", "--output=carrier-pigeon"}},
		{"a timeout that cannot elapse", []string{"--node=n", "--output=stdout", "--timeout=0"}},
	} {
		if _, err := parseOptions(tc.args, noEnv); err == nil {
			t.Errorf("%s: accepted it anyway", tc.name)
		}
	}
}
