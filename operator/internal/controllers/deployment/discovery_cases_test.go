// The case tree under testdata/discovery, driven through the discovery run.
//
// Each directory there is a fleet: the probe reports, the node objects, and the
// OperatorOps a run is started from. This drives every one of them through the
// step that turns reports into a document, and compares what came out against
// what the directory says should.
//
// It goes through the reconciler rather than through the planner because the
// document is the product. What a reviewer approves is a
// ClusterDeploymentConfig, and the parts of it the planner has no hand in are
// exactly the parts a fixture is worth having for: the name, the environment,
// the cluster block, and whether the run refused outright.
//
// The expectations are files rather than assertions in Go. A case that produces
// a different document produces a diff a reviewer reads, which is the only form
// in which 171 outcomes can be reviewed at all. The same run with -update
// rewrites them, so recording a new case is not transcription.
//
// What that buys is a record of today's behavior and nothing more. A recorded
// file is evidence that the output has not changed since somebody read it; it
// is not evidence that the output is right. Reading each one against the row in
// its case.md is the step that supplies the second, and it is a step no harness
// can do.

package deployment

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// update rewrites every expectation from what the run produced, for recording a
// case rather than checking one.
var update = flag.Bool("update", false,
	"rewrite the expected files of every discovery case from what the run produced")

// caseRoot is the tree the cases live in.
const caseRoot = "testdata/discovery"

// The files a case directory holds. The inputs are written by
// hack/discoveryfixtures and the expectations by this test.
const (
	caseFile     = "case.md"
	opsFile      = "ops.yaml"
	nodesFile    = "nodes.yaml"
	reportsDir   = "reports"
	expectedFile = "expected.yaml"
	refusalsFile = "expected-refusals.txt"
	notesFile    = "expected-notes.txt"
	errorFile    = "expected-error.txt"
)

// discoveryCase is one directory, loaded.
type discoveryCase struct {
	// Name is the path under the case root, which is what a failure names.
	Name string

	// Dir is where the case's files are.
	Dir string

	// Ops is the run, and Objects is everything the run reads: the node
	// objects and the ConfigMaps the probes wrote.
	Ops     *simplyblockv1alpha2.OperatorOps
	Objects []client.Object
}

func TestDiscoveryCases(t *testing.T) {
	cases := loadDiscoveryCases(t)
	if len(cases) == 0 {
		t.Fatalf("no cases under %s: the tree is written by hack/discoveryfixtures", caseRoot)
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			outcome := runDiscoveryCase(t, c)
			compareDiscoveryOutcome(t, c, outcome)
		})
	}
}

// outcome is everything one case produced.
type outcome struct {
	// Config is the document the run wrote, or nil when it wrote none.
	Config *simplyblockv1alpha2.ClusterDeploymentConfig

	// Refusals are the device and worker refusals the run reported, and Notes
	// are the sentences accounting for the numbers in the cluster block. Both
	// reach a reviewer as events, which is where they are read from.
	Refusals []string
	Notes    []string

	// Failure is why the run wrote no document.
	Failure string
}

// runDiscoveryCase drives one case through the writing step.
func runDiscoveryCase(t *testing.T, c discoveryCase) outcome {
	t.Helper()

	scheme := opsScheme(t)
	ops := c.Ops.DeepCopy()
	objects := append([]client.Object{ops}, c.Objects...)

	recorder := events.NewFakeRecorder(1024)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.OperatorOps{}).
		Build()

	reconciler := &OperatorOpsReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: recorder, ProbeImage: opsImage,
	}

	_, err := reconciler.write(context.Background(), ops)

	out := outcome{}
	if err != nil {
		out.Failure = err.Error()
	}

	// The document is read back from the client rather than taken from the
	// reconciler, so that what is compared is what a reviewer would get.
	var written simplyblockv1alpha2.ClusterDeploymentConfigList
	if listErr := fakeClient.List(context.Background(), &written); listErr != nil {
		t.Fatalf("list the documents the run wrote: %v", listErr)
	}
	switch len(written.Items) {
	case 0:
	case 1:
		out.Config = &written.Items[0]
	default:
		t.Fatalf("the run wrote %d documents, and a run writes one", len(written.Items))
	}

	out.Refusals, out.Notes = drainEvents(recorder)
	return out
}

// drainEvents separates what the run said into the refusals and the notes,
// which are the two things a reviewer is owed and are read from different
// events.
func drainEvents(recorder *events.FakeRecorder) (refusals, notes []string) {
	for {
		select {
		case line := <-recorder.Events:
			message := eventMessage(line)
			switch {
			case strings.Contains(line, DeviceDeclined):
				refusals = append(refusals, message)
			case strings.Contains(line, ConfigWritten):
				notes = append(notes, message)
			}
		default:
			return refusals, notes
		}
	}
}

// eventMessage is the message out of a recorded event, which the fake renders
// as a single line with the type and the reason ahead of it.
func eventMessage(line string) string {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 3 {
		return line
	}
	return parts[2]
}

// compareDiscoveryOutcome checks one case against its recorded expectations, or
// records them.
func compareDiscoveryOutcome(t *testing.T, c discoveryCase, got outcome) {
	t.Helper()

	document := ""
	if got.Config != nil {
		rendered, err := yaml.Marshal(comparableConfig(got.Config))
		if err != nil {
			t.Fatalf("render the document: %v", err)
		}
		document = string(rendered)
	}

	files := map[string]string{
		expectedFile: document,
		refusalsFile: lines(got.Refusals),
		notesFile:    lines(got.Notes),
		errorFile:    trailing(got.Failure),
	}

	if *update {
		for name, content := range files {
			path := filepath.Join(c.Dir, name)
			if content == "" {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					t.Fatalf("remove %s: %v", path, err)
				}
				continue
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
		return
	}

	for name, content := range files {
		path := filepath.Join(c.Dir, name)
		want, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(want) == content {
			continue
		}
		t.Errorf("%s does not match.\n--- recorded ---\n%s\n--- produced ---\n%s\n"+
			"Run `go test ./internal/controllers/deployment/ -run TestDiscoveryCases -update` "+
			"to record this, and read the result against the row in case.md before committing it.",
			name, want, content)
	}
}

// comparableConfig is the document with the fields a fake client invents
// stripped, so that a diff is about the draft rather than about the harness.
func comparableConfig(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) *simplyblockv1alpha2.ClusterDeploymentConfig {
	out := config.DeepCopy()
	out.ResourceVersion = ""
	out.CreationTimestamp = metav1.Time{}
	out.ManagedFields = nil
	out.UID = ""
	out.Generation = 0
	out.TypeMeta = metav1.TypeMeta{
		APIVersion: simplyblockv1alpha2.GroupVersion.String(),
		Kind:       "ClusterDeploymentConfig",
	}
	return out
}

// lines renders a list as a file, one entry per line.
func lines(entries []string) string {
	if len(entries) == 0 {
		return ""
	}
	return strings.Join(entries, "\n") + "\n"
}

// trailing renders a single value as a file, and nothing for an empty one.
func trailing(value string) string {
	if value == "" {
		return ""
	}
	return value + "\n"
}

// loadDiscoveryCases reads every case directory under the root.
//
// A directory carrying a case.md is a case, which is what lets the tree be
// organized into families without the loader knowing what a family is.
func loadDiscoveryCases(t *testing.T) []discoveryCase {
	t.Helper()

	var cases []discoveryCase
	err := filepath.WalkDir(caseRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != caseFile {
			return nil
		}

		dir := filepath.Dir(path)
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// A case whose harness is the discovery package has no objects to be
		// driven from, and its directory carries the row alone.
		if strings.Contains(string(body), "**Harness.** `GO`") {
			return nil
		}

		loaded, err := loadDiscoveryCase(dir)
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		loaded.Name = strings.TrimPrefix(dir, caseRoot+string(filepath.Separator))
		cases = append(cases, loaded)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", caseRoot, err)
	}

	sort.Slice(cases, func(i, j int) bool { return cases[i].Name < cases[j].Name })
	return cases
}

// loadDiscoveryCase reads one directory's objects.
func loadDiscoveryCase(dir string) (discoveryCase, error) {
	out := discoveryCase{Dir: dir}

	opsRaw, err := os.ReadFile(filepath.Join(dir, opsFile))
	if err != nil {
		return out, fmt.Errorf("read the run: %w", err)
	}
	var ops simplyblockv1alpha2.OperatorOps
	if err := yaml.Unmarshal(opsRaw, &ops); err != nil {
		return out, fmt.Errorf("parse the run: %w", err)
	}
	out.Ops = &ops

	nodes, err := readDocuments[corev1.Node](filepath.Join(dir, nodesFile))
	if err != nil {
		return out, fmt.Errorf("read the nodes: %w", err)
	}
	for i := range nodes {
		out.Objects = append(out.Objects, &nodes[i])
	}

	entries, err := os.ReadDir(filepath.Join(dir, reportsDir))
	if err != nil && !os.IsNotExist(err) {
		return out, fmt.Errorf("list the reports: %w", err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, reportsDir, entry.Name()))
		if err != nil {
			return out, err
		}
		var cm corev1.ConfigMap
		if err := yaml.Unmarshal(raw, &cm); err != nil {
			return out, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		out.Objects = append(out.Objects, &cm)
	}
	return out, nil
}

// readDocuments reads a multi-document file, and nothing for a file that is not
// there: a case with no node objects is a run whose workers Kubernetes says
// nothing about.
func readDocuments[T any](path string) ([]T, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []T
	for _, document := range strings.Split(string(raw), "\n---\n") {
		if strings.TrimSpace(document) == "" {
			continue
		}
		var object T
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			return nil, err
		}
		out = append(out, object)
	}
	return out, nil
}
