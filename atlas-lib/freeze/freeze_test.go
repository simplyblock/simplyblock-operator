package freeze

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeMapper records what was suspended and resumed, in order.
type fakeMapper struct {
	mu         sync.Mutex
	calls      []string
	suspendErr error
	resumeErr  error
	onSuspend  func()
}

func (m *fakeMapper) Suspend(_ context.Context, name string) error {
	m.mu.Lock()
	m.calls = append(m.calls, "suspend "+name)
	hook := m.onSuspend
	err := m.suspendErr
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (m *fakeMapper) Resume(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "resume "+name)
	return m.resumeErr
}

func (m *fakeMapper) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

// fakeTimer is a deadline the test fires by hand.
type fakeTimer struct {
	mu      sync.Mutex
	f       func()
	d       time.Duration
	stopped bool
	fired   bool
}

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

func (t *fakeTimer) fire() {
	t.mu.Lock()
	if t.stopped || t.fired {
		t.mu.Unlock()
		return
	}
	t.fired = true
	t.mu.Unlock()
	t.f()
}

// newHolds returns holds over a fake mapper whose deadlines the test fires.
func newHolds(t *testing.T, dir string) (*Holds, *fakeMapper, *[]*fakeTimer) {
	t.Helper()
	m := &fakeMapper{}
	var timers []*fakeTimer
	h := New(Config{
		Mapper:      m,
		Dir:         dir,
		MaxDeadline: 10 * time.Second,
		AfterFunc: func(d time.Duration, f func()) Timer {
			ft := &fakeTimer{f: f, d: d}
			timers = append(timers, ft)
			return ft
		},
	})
	return h, m, &timers
}

func TestFreezeThenThawHoldsTheWholeTime(t *testing.T) {
	dir := t.TempDir()
	h, m, timers := newHolds(t, dir)
	released := 0
	acquire := func(context.Context) (func(), error) { return func() { released++ }, nil }

	if err := h.Freeze(context.Background(), "vol", "dm-x", "tok", 5*time.Second, acquire); err != nil {
		t.Fatal(err)
	}
	if (*timers)[0].d != 5*time.Second {
		t.Errorf("deadline = %v, want the 5s asked for", (*timers)[0].d)
	}
	if _, err := os.Stat(filepath.Join(dir, "vol.json")); err != nil {
		t.Errorf("the hold is not recorded on disk while it lasts: %v", err)
	}
	held, err := h.Thaw(context.Background(), "vol", "tok")
	if !held || err != nil {
		t.Fatalf("Thaw = %v, %v; want a hold that lasted", held, err)
	}
	if got := m.seen(); !slices.Equal(got, []string{"suspend dm-x", "resume dm-x"}) {
		t.Errorf("calls = %v", got)
	}
	if released != 1 {
		t.Errorf("released %d times, want once", released)
	}
	if _, err := os.Stat(filepath.Join(dir, "vol.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the record outlived the hold: %v", err)
	}
}

// The deadline thaws without the holder, and a late Thaw learns the hold did
// not last, which is what makes its holder throw away what it took.
func TestTheDeadlineThawsAndALateThawIsTold(t *testing.T) {
	h, m, timers := newHolds(t, "")
	if err := h.Freeze(context.Background(), "vol", "dm-x", "tok", time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if (*timers)[0].d != 10*time.Second {
		t.Errorf("deadline = %v, want the host's 10s cap", (*timers)[0].d)
	}
	(*timers)[0].fire()
	if got := m.seen(); !slices.Equal(got, []string{"suspend dm-x", "resume dm-x"}) {
		t.Fatalf("the deadline did not resume the device: %v", got)
	}
	held, err := h.Thaw(context.Background(), "vol", "tok")
	if held || err != nil {
		t.Errorf("Thaw after the deadline = %v, %v; want false, nil", held, err)
	}
	if got := m.seen(); len(got) != 2 {
		t.Errorf("a late Thaw resumed again: %v", got)
	}
}

func TestATokenMismatchTouchesNothing(t *testing.T) {
	h, m, _ := newHolds(t, "")
	if err := h.Freeze(context.Background(), "vol", "dm-x", "mine", 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.Freeze(context.Background(), "vol", "dm-x", "theirs", 0, nil); !errors.Is(err, ErrHeld) {
		t.Errorf("a second holder's Freeze = %v, want ErrHeld", err)
	}
	if held, err := h.Thaw(context.Background(), "vol", "theirs"); held || !errors.Is(err, ErrTokenMismatch) {
		t.Errorf("a second holder's Thaw = %v, %v; want ErrTokenMismatch", held, err)
	}
	if got := m.seen(); !slices.Equal(got, []string{"suspend dm-x"}) {
		t.Errorf("another token suspended or resumed: %v", got)
	}
	if held, err := h.Thaw(context.Background(), "vol", "mine"); !held || err != nil {
		t.Errorf("the holder's Thaw = %v, %v", held, err)
	}
}

// A retried Freeze under the same token is the same hold: no second suspend.
func TestARetriedFreezeIsTheSameHold(t *testing.T) {
	h, m, _ := newHolds(t, "")
	for range 2 {
		if err := h.Freeze(context.Background(), "vol", "dm-x", "tok", 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.seen(); !slices.Equal(got, []string{"suspend dm-x"}) {
		t.Errorf("calls = %v, want one suspend", got)
	}
}

// A suspend that fails is resumed anyway, in case it took effect partway, and
// the volume is free for the next attempt.
func TestAFailedSuspendResumesAndReleases(t *testing.T) {
	h, m, _ := newHolds(t, "")
	m.suspendErr = errors.New("ioctl failed")
	released := 0
	acquire := func(context.Context) (func(), error) { return func() { released++ }, nil }
	if err := h.Freeze(context.Background(), "vol", "dm-x", "tok", 0, acquire); err == nil {
		t.Fatal("a failed suspend was reported as a freeze")
	}
	if got := m.seen(); !slices.Equal(got, []string{"suspend dm-x", "resume dm-x"}) {
		t.Errorf("calls = %v", got)
	}
	if released != 1 {
		t.Errorf("released %d times", released)
	}
	m.suspendErr = nil
	if err := h.Freeze(context.Background(), "vol", "dm-x", "other", 0, nil); err != nil {
		t.Errorf("the volume stayed held after a failed freeze: %v", err)
	}
}

// A Thaw that arrives while the suspend is still running cancels it: the
// device goes straight back, and the holder is told the freeze did not hold.
func TestAThawDuringTheSuspendCancelsIt(t *testing.T) {
	h, m, _ := newHolds(t, "")
	m.onSuspend = func() {
		if held, err := h.Thaw(context.Background(), "vol", "tok"); held || err != nil {
			t.Errorf("Thaw during the suspend = %v, %v", held, err)
		}
	}
	if err := h.Freeze(context.Background(), "vol", "dm-x", "tok", 0, nil); err == nil {
		t.Error("a freeze thawed during its suspend reported success")
	}
	if got := m.seen(); !slices.Equal(got, []string{"suspend dm-x", "resume dm-x"}) {
		t.Errorf("calls = %v", got)
	}
}

// A process that restarts with a hold recorded resumes the device, since the
// deadline that would have done so went with the previous process.
func TestRecoverResumesWhatAPreviousProcessLeftFrozen(t *testing.T) {
	dir := t.TempDir()
	first, _, _ := newHolds(t, dir)
	if err := first.Freeze(context.Background(), "vol", "dm-x", "tok", 0, nil); err != nil {
		t.Fatal(err)
	}

	second, m, _ := newHolds(t, dir)
	if err := second.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.seen(); !slices.Equal(got, []string{"resume dm-x"}) {
		t.Errorf("calls = %v, want the left-over device resumed", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("records remain after recovery: %v", entries)
	}
}
