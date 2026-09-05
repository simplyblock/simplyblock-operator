// What bringing a stack up and down has to guarantee.
//
// Most of these are about order and about what is not called. A failed bring-up
// that reached for Destroy would remove a volume group holding data that a
// misfiring format check failed to see, and a record written after the first
// side effect leaves paths attached that nothing will ever release.

package volstack

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
)

// fakeLayer records what was called on it and answers what a test told it to.
type fakeLayer struct {
	name  string
	log   *[]string
	state State

	observeErr error
	ensureErr  error
	releaseErr error

	// exposes is the device this layer hands upward, so a test can assert that
	// each layer received what the one below it produced.
	exposes string

	healthy  bool
	healErr  error
	grownTo  string
	observed []string // the device names this layer was handed, per Observe
}

func (f *fakeLayer) Name() string { return f.name }

func (f *fakeLayer) note(verb string) { *f.log = append(*f.log, f.name+":"+verb) }

func (f *fakeLayer) Observe(_ context.Context, below Artifact) (State, error) {
	f.note("observe")
	if d, ok := below.Device(); ok {
		f.observed = append(f.observed, d.Name)
	} else {
		f.observed = append(f.observed, "")
	}
	return f.state, f.observeErr
}

func (f *fakeLayer) Ensure(_ context.Context, below Artifact) (Artifact, error) {
	f.note("ensure")
	if f.ensureErr != nil {
		return Artifact{}, f.ensureErr
	}
	if f.exposes == "" {
		return below, nil
	}
	return Artifact{Devices: []blockdev.Device{{Name: f.exposes, Path: "/dev/" + f.exposes, SizeBytes: 1 << 30}}}, nil
}

func (f *fakeLayer) Release(_ context.Context, _ Artifact) error {
	f.note("release")
	return f.releaseErr
}

func (f *fakeLayer) Destroy(_ context.Context, _ Artifact) error {
	f.note("destroy")
	return nil
}

// healingLayer adds the optional Healer interface.
type healingLayer struct {
	*fakeLayer
}

func (h healingLayer) Healthy(context.Context, Artifact) (bool, error) {
	h.note("healthy")
	return h.healthy, nil
}

func (h healingLayer) Heal(context.Context, Artifact, Artifact) error {
	h.note("heal")
	return h.healErr
}

// growingLayer adds the optional Grower interface.
type growingLayer struct {
	*fakeLayer
}

func (g growingLayer) Grow(_ context.Context, below Artifact) (Artifact, error) {
	g.note("grow")
	if g.grownTo == "" {
		return below, nil
	}
	return Artifact{Devices: []blockdev.Device{{Name: g.grownTo}}}, nil
}

func newRunner(t *testing.T) *Runner {
	t.Helper()
	return NewRunner(NewStore(t.TempDir()))
}

// Up walks the plan bottom to top, observing each layer before ensuring it.
func TestUpWalksBottomToTop(t *testing.T) {
	var log []string
	plan := Plan{
		&fakeLayer{name: "fabric", log: &log, exposes: "nvme0n1"},
		&fakeLayer{name: "filesystem", log: &log},
	}

	r := newRunner(t)
	if _, err := r.Up(context.Background(), testHandle, plan); err != nil {
		t.Fatalf("Up: %v", err)
	}

	want := "fabric:observe fabric:ensure filesystem:observe filesystem:ensure"
	if got := strings.Join(log, " "); got != want {
		t.Errorf("call order:\n got %s\nwant %s", got, want)
	}
}

// Each layer receives what the layer below it produced, which is the whole point
// of the artifact traveling upward rather than each layer resolving a path.
func TestUpPassesTheArtifactUpward(t *testing.T) {
	var log []string
	bottom := &fakeLayer{name: "fabric", log: &log, exposes: "nvme0n1"}
	top := &fakeLayer{name: "filesystem", log: &log}

	r := newRunner(t)
	if _, err := r.Up(context.Background(), testHandle, Plan{bottom, top}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if len(top.observed) != 1 || top.observed[0] != "nvme0n1" {
		t.Fatalf("the top layer observed %v, want the device the bottom exposed", top.observed)
	}
}

// The record is written before the first Ensure. A crash between a fabric
// connect and the recording of it leaves paths attached that nothing will ever
// release, which this repository has already paid for once.
func TestUpWritesTheRecordBeforeTheFirstSideEffect(t *testing.T) {
	store := NewStore(t.TempDir())
	r := NewRunner(store)

	var log []string
	var recordAtEnsure *Record
	probe := &recordingLayer{
		fakeLayer: &fakeLayer{name: "fabric", log: &log},
		onEnsure: func() {
			rec, err := store.Load(testHandle)
			if err == nil {
				recordAtEnsure = &rec
			}
		},
	}

	if _, err := r.Up(context.Background(), testHandle, Plan{probe}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if recordAtEnsure == nil {
		t.Fatal("no record existed when the first Ensure ran, so a crash there would strand the stack")
	}
	if len(recordAtEnsure.Plan) != 1 || recordAtEnsure.Plan[0].Layer != "fabric" {
		t.Errorf("the record written before the first Ensure did not hold the plan: %+v", recordAtEnsure.Plan)
	}
	if !recordAtEnsure.Plan[0].Attempted {
		t.Error("the layer was not marked attempted before its own Ensure ran")
	}
}

// recordingLayer runs a callback inside Ensure, so a test can look at the world
// as the layer sees it.
type recordingLayer struct {
	*fakeLayer
	onEnsure func()
}

func (l *recordingLayer) Ensure(ctx context.Context, below Artifact) (Artifact, error) {
	if l.onEnsure != nil {
		l.onEnsure()
	}
	return l.fakeLayer.Ensure(ctx, below)
}

// A failed bring-up releases what it brought up, top-down, and never destroys.
// This is the rule the four verbs exist for.
func TestFailedUpUnwindsWithReleaseAndNeverDestroys(t *testing.T) {
	var log []string
	plan := Plan{
		&fakeLayer{name: "fabric", log: &log, exposes: "nvme0n1"},
		&fakeLayer{name: "lvm", log: &log, exposes: "dm-0"},
		&fakeLayer{name: "filesystem", log: &log, ensureErr: errors.New("mkfs refused the device")},
	}

	r := newRunner(t)
	if _, err := r.Up(context.Background(), testHandle, plan); err == nil {
		t.Fatal("Up returned no error although a layer failed")
	}

	joined := strings.Join(log, " ")
	if strings.Contains(joined, ":destroy") {
		t.Fatalf("a failed bring-up called Destroy, which removes data a failed format check may have misjudged:\n%s", joined)
	}
	if !strings.Contains(joined, "lvm:release") || !strings.Contains(joined, "fabric:release") {
		t.Fatalf("the layers already brought up were not released:\n%s", joined)
	}
	lvmAt, fabricAt := indexOf(log, "lvm:release"), indexOf(log, "fabric:release")
	if lvmAt > fabricAt {
		t.Errorf("unwind released bottom-up; it has to go top-down:\n%s", joined)
	}
}

// The record survives a failed bring-up, so even a process that never retries
// leaves a removable stack behind.
func TestFailedUpKeepsTheRecord(t *testing.T) {
	store := NewStore(t.TempDir())
	r := NewRunner(store)
	var log []string
	plan := Plan{&fakeLayer{name: "fabric", log: &log, ensureErr: errors.New("no path")}}

	if _, err := r.Up(context.Background(), testHandle, plan); err == nil {
		t.Fatal("Up returned no error")
	}
	if _, err := store.Load(testHandle); err != nil {
		t.Fatalf("the record is gone after a failed bring-up: %v", err)
	}
}

// Down walks top-down and releases, and removes the record when it is finished.
func TestDownReleasesTopDownAndRemovesTheRecord(t *testing.T) {
	store := NewStore(t.TempDir())
	r := NewRunner(store)

	var log []string
	plan := Plan{
		&fakeLayer{name: "fabric", log: &log, exposes: "nvme0n1", state: StateReady},
		&fakeLayer{name: "filesystem", log: &log, state: StateReady},
	}
	if _, err := r.Up(context.Background(), testHandle, plan); err != nil {
		t.Fatalf("Up: %v", err)
	}
	log = nil

	if err := r.Down(context.Background(), testHandle, plan); err != nil {
		t.Fatalf("Down: %v", err)
	}

	joined := strings.Join(log, " ")
	if indexOf(log, "filesystem:release") > indexOf(log, "fabric:release") {
		t.Errorf("Down released bottom-up:\n%s", joined)
	}
	if strings.Contains(joined, ":destroy") {
		t.Fatalf("Down called Destroy, which an unstage must never reach:\n%s", joined)
	}
	if _, err := store.Load(testHandle); !errors.Is(err, ErrNoRecord) {
		t.Errorf("the record survived a completed teardown: %v", err)
	}
}

// A layer whose Observe reports StateAbsent has nothing to release, and skipping
// it is what lets a teardown run against a stack that was never fully built.
func TestDownSkipsAbsentLayers(t *testing.T) {
	store := NewStore(t.TempDir())
	r := NewRunner(store)

	var log []string
	plan := Plan{
		&fakeLayer{name: "fabric", log: &log, exposes: "nvme0n1", state: StateReady},
		&fakeLayer{name: "filesystem", log: &log, state: StateAbsent},
	}
	if _, err := r.Up(context.Background(), testHandle, plan); err != nil {
		t.Fatalf("Up: %v", err)
	}
	log = nil

	if err := r.Down(context.Background(), testHandle, plan); err != nil {
		t.Fatalf("Down: %v", err)
	}
	joined := strings.Join(log, " ")
	if strings.Contains(joined, "filesystem:release") {
		t.Errorf("an absent layer was released:\n%s", joined)
	}
	if !strings.Contains(joined, "fabric:release") {
		t.Errorf("the layer that was present was not released:\n%s", joined)
	}
}

// Heal asks each Healer whether it is serving and repairs only what is not. A
// layer that implements no Healer is skipped rather than special-cased.
func TestHealRepairsOnlyUnhealthyLayers(t *testing.T) {
	var log []string
	fabric := &fakeLayer{name: "fabric", log: &log, exposes: "nvme0n1", state: StateReady, healthy: false}
	plain := &fakeLayer{name: "plain", log: &log, state: StateReady}
	fs := &fakeLayer{name: "filesystem", log: &log, state: StateReady, healthy: true}

	plan := Plan{healingLayer{fabric}, plain, healingLayer{fs}}
	r := newRunner(t)
	if _, err := r.Up(context.Background(), testHandle, plan); err != nil {
		t.Fatalf("Up: %v", err)
	}
	log = nil

	if err := r.Heal(context.Background(), testHandle, plan); err != nil {
		t.Fatalf("Heal: %v", err)
	}

	// Matched as whole entries, because `filesystem:heal` is a prefix of
	// `filesystem:healthy` and a substring test passes for the wrong reason.
	joined := strings.Join(log, " ")
	if indexOf(log, "fabric:heal") < 0 {
		t.Errorf("the unhealthy layer was not healed:\n%s", joined)
	}
	if indexOf(log, "filesystem:heal") >= 0 {
		t.Errorf("a healthy layer was healed anyway:\n%s", joined)
	}
	if indexOf(log, "plain:heal") >= 0 || indexOf(log, "plain:healthy") >= 0 {
		t.Errorf("a layer implementing no Healer was asked to heal:\n%s", joined)
	}
}

// Grow runs bottom to top and skips layers that implement no Grower, which is
// what makes a plan with no Grower at all the correct answer for a volume that
// needs no node-side expansion.
func TestGrowSkipsLayersThatCannotGrow(t *testing.T) {
	var log []string
	bottom := &fakeLayer{name: "fabric", log: &log, exposes: "nvme0n1"}
	middle := &fakeLayer{name: "plain", log: &log}
	top := &fakeLayer{name: "filesystem", log: &log}

	plan := Plan{growingLayer{bottom}, middle, growingLayer{top}}
	r := newRunner(t)
	if err := r.Grow(context.Background(), plan); err != nil {
		t.Fatalf("Grow: %v", err)
	}

	joined := strings.Join(log, " ")
	if !strings.Contains(joined, "fabric:grow") || !strings.Contains(joined, "filesystem:grow") {
		t.Fatalf("a Grower was skipped:\n%s", joined)
	}
	if strings.Contains(joined, "plain:grow") {
		t.Fatalf("a layer implementing no Grower was grown:\n%s", joined)
	}
	if indexOf(log, "fabric:grow") > indexOf(log, "filesystem:grow") {
		t.Errorf("Grow ran top-down; it has to go bottom-up:\n%s", joined)
	}
}

// Destroy is reachable only from a deletion path, and it runs top-down so a
// layer is removed before what it sits on.
func TestDestroyRunsTopDown(t *testing.T) {
	var log []string
	plan := Plan{
		&fakeLayer{name: "fabric", log: &log, exposes: "nvme0n1", state: StateReady},
		&fakeLayer{name: "lvm", log: &log, state: StateReady},
	}
	r := newRunner(t)
	if err := r.Destroy(context.Background(), testHandle, plan); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	joined := strings.Join(log, " ")
	if indexOf(log, "lvm:destroy") > indexOf(log, "fabric:destroy") {
		t.Errorf("Destroy ran bottom-up:\n%s", joined)
	}
}

func indexOf(log []string, want string) int {
	for i, s := range log {
		if s == want {
			return i
		}
	}
	return -1
}
