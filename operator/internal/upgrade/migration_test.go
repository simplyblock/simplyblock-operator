// Tests for the migrate walk and for the record it keeps its position in. The
// property under test throughout is §22's: a run killed anywhere is safe to
// rerun, because the position is derived from the cluster and the one thing
// that is not derivable is written down before the side effect rather than
// after it.

package upgrade

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/statemachine"
)

// migrateStep is a step registered into one migrate phase.
func migrateStep(id ID, phase Phase) *recordingStep {
	step := newStep(id, StageMigrate)
	step.phase = phase
	return step
}

// migrationFixture builds a migration over an empty fake cluster.
func migrationFixture(t *testing.T, steps ...*recordingStep) (*Migration, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	catalog := NewCatalog()
	for _, step := range steps {
		catalog.Steps.MustRegister(step)
	}

	scope := NewScope(c, "simplyblock", StageMigrate, Options{}, logf.Log, DiscardReporter{})
	return NewMigration(NewRunner(catalog, scope), NewRecordStore(c, "simplyblock")), c
}

func TestMigration_WalksEveryPhaseAndRemovesTheRecord(t *testing.T) {
	transform := migrateStep("copy-renamed-kinds", PhaseTransforming)
	reparent := migrateStep("reparent-node-set-children", PhaseOwnership)
	migration, c := migrationFixture(t, transform, reparent)

	if err := migration.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if transform.applied != 1 || reparent.applied != 1 {
		t.Fatalf("applied transform %d times and reparent %d, want 1 each",
			transform.applied, reparent.applied)
	}

	// A completed migration deletes its record, so a later run starts from the
	// cluster rather than from a position that says it is finished.
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: "simplyblock", Name: RecordName}
	if err := c.Get(t.Context(), key, &cm); err == nil {
		t.Fatal("the record survived a completed migration")
	}
}

func TestMigration_StopsAtTheFailingPhaseAndKeepsThePositionBeforeIt(t *testing.T) {
	failing := migrateStep("copy-renamed-kinds", PhaseTransforming)
	failing.applyErr = errors.New("the API server refused the copy")
	later := migrateStep("reparent-node-set-children", PhaseOwnership)

	migration, c := migrationFixture(t, failing, later)

	if err := migration.Run(t.Context()); err == nil {
		t.Fatal("a failing phase did not stop the migration")
	}
	if later.applied != 0 {
		t.Fatal("a phase after the one that failed ran, which is what §25 refuses")
	}

	// The record holds the last phase that completed, so a rerun re-enters the
	// one that failed rather than resuming past it.
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: "simplyblock", Name: RecordName}
	if err := c.Get(t.Context(), key, &cm); err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if got := cm.Data["phase"]; got != string(PhaseValidating) {
		t.Fatalf("phase = %q, want %q: the failing phase was never entered",
			got, PhaseValidating)
	}

	// The step that was in flight is named, which is the one position the
	// cluster does not answer for (§22.1).
	if got := cm.Data["step"]; got != string(failing.ID()) {
		t.Fatalf("step = %q, want the step that was in flight, %q", got, failing.ID())
	}
}

func TestMigration_ResumesWithoutRepeatingWhatIsDone(t *testing.T) {
	transform := migrateStep("copy-renamed-kinds", PhaseTransforming)
	transform.applyErr = errors.New("interrupted")
	migration, _ := migrationFixture(t, transform)

	if err := migration.Run(t.Context()); err == nil {
		t.Fatal("the first run was expected to fail")
	}

	// The step's effect is now present, which is what the second run's Done
	// reports. Nothing about the record says so: it is a read of the cluster.
	transform.applyErr = nil
	transform.done = true

	if err := migration.Run(t.Context()); err != nil {
		t.Fatalf("the resumed run failed: %v", err)
	}
	if transform.applied != 1 {
		t.Fatalf("Apply was called %d times across two runs, want 1: a resumed run "+
			"must not repeat a side effect", transform.applied)
	}
}

func TestMigration_RefusesAPhaseThisBuildDoesNotDeclare(t *testing.T) {
	migration, c := migrationFixture(t)

	// A record written by a build that declared a phase this one does not: a
	// downgrade, or a hand-edited ConfigMap.
	stale := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: RecordName, Namespace: "simplyblock"},
		Data:       map[string]string{"phase": "Reticulating"},
	}
	if err := c.Create(t.Context(), stale); err != nil {
		t.Fatalf("writing the stale record: %v", err)
	}

	err := migration.Run(t.Context())
	if err == nil {
		t.Fatal("a record naming an undeclared phase was accepted, and the walk " +
			"would have restarted from the beginning")
	}
	if !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("error = %q, want it to say the phase is not declared", err)
	}
}

func TestMigrateGraph_DeclaresEveryEdgeItUses(t *testing.T) {
	// statemachine.New validates that no edge points at an undeclared state,
	// which is what keeps an unreachable phase from surfacing at runtime on the
	// unhappy path.
	machine, err := statemachine.New(t.Context(), MigrateGraph(nil))
	if err != nil {
		t.Fatalf("the migrate graph is not closed: %v", err)
	}
	defer machine.Close()

	if machine.CurrentState() != PhasePending {
		t.Fatalf("a new machine starts in %q, want %q", machine.CurrentState(), PhasePending)
	}
}

func TestMigrateGraph_EveryPhaseCanFail(t *testing.T) {
	config := MigrateGraph(nil)
	for _, phase := range MigratePhases {
		if phase.Terminal() {
			continue
		}
		if !containsPhase(config.States[phase].To, PhaseFailed) {
			t.Fatalf("%s cannot reach %s, so a refusal there has nowhere to go",
				phase, PhaseFailed)
		}
	}
}

func TestMigrateGraph_TerminalPhasesAreTerminal(t *testing.T) {
	config := MigrateGraph(nil)
	for _, phase := range []Phase{PhaseCompleted, PhaseFailed} {
		if len(config.States[phase].To) != 0 {
			t.Fatalf("%s has outgoing edges, and a finished migration could be walked on", phase)
		}
	}
}

func TestRecord_RoundTripsThroughTheCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := NewRecordStore(c, "simplyblock")

	written := &Record{
		Snapshot:  statemachine.Snapshot[Phase]{State: PhaseOwnership},
		Step:      "reparent-node-set-children",
		Preflight: &PreflightAttestation{Passed: metav1.Now(), ChartVersion: "1.2.3"},
	}
	if err := store.Save(t.Context(), written); err != nil {
		t.Fatalf("Save: %v", err)
	}

	read, existed, err := store.Load(t.Context())
	if err != nil || !existed {
		t.Fatalf("Load: %v, existed=%v", err, existed)
	}
	if read.Snapshot.State != PhaseOwnership {
		t.Fatalf("phase = %q, want %q", read.Snapshot.State, PhaseOwnership)
	}
	if read.Step != written.Step {
		t.Fatalf("step = %q, want %q", read.Step, written.Step)
	}
	if read.Preflight == nil || read.Preflight.ChartVersion != "1.2.3" {
		t.Fatalf("preflight attestation = %+v, want the chart version it was written with", read.Preflight)
	}
}

func TestRecord_AbsentIsNotAnError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	store := NewRecordStore(fake.NewClientBuilder().WithScheme(scheme).Build(), "simplyblock")

	record, existed, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if existed || record != nil {
		t.Fatal("a cluster with no record reported one: a migration that has not " +
			"started is not an error")
	}
}

func TestRecord_HoldsTheOptimisticLock(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := NewRecordStore(c, "simplyblock")

	first := &Record{Snapshot: statemachine.Snapshot[Phase]{State: PhaseValidating}}
	if err := store.Save(t.Context(), first); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A second run loads the record and a third writes before it does.
	stale, _, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	first.Snapshot.State = PhaseOwnership
	if err := store.Save(t.Context(), first); err != nil {
		t.Fatalf("the concurrent write failed: %v", err)
	}

	stale.Snapshot.State = PhaseDeleting
	if err := store.Save(t.Context(), stale); err == nil {
		t.Fatal("a stale write was accepted, so two concurrent runs could both " +
			"believe they held the migration")
	}
}

// containsPhase reports membership, which slices.Contains would too but for a
// named type the test reads better with.
func containsPhase(phases []Phase, want Phase) bool {
	for _, phase := range phases {
		if phase == want {
			return true
		}
	}
	return false
}
