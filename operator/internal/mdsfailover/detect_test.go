// Tests of the detection window, the target choice, the API read retry, and
// the record write: O-18 to O-23, O-35 to O-40, and the record half of O-08
// (design-pnfs-mds-standby.md §7.2, §7.3, §4.3).

package mdsfailover

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

// fakeClock is time that only moves when a test or a Sleep moves it.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

var lostConditions = Conditions{NoSession: true, Unready: true, StandbyUp: true}

func TestO18ALossShorterThanTheWindowIsHeld(t *testing.T) {
	clk := newFakeClock()
	d := NewDetector(20 * time.Second)

	if got := d.Judge(clk.Now(), lostConditions); got != VerdictHeld {
		t.Fatalf("first observation = %v, want held", got)
	}
	clk.advance(10 * time.Second)
	if got := d.Judge(clk.Now(), lostConditions); got != VerdictHeld {
		t.Errorf("10s into a 20s window = %v, want held", got)
	}
}

func TestO19TheWindowIsExclusiveAtItsEnd(t *testing.T) {
	clk := newFakeClock()
	d := NewDetector(20 * time.Second)
	d.Judge(clk.Now(), lostConditions)

	clk.advance(20 * time.Second)
	if got := d.Judge(clk.Now(), lostConditions); got != VerdictHeld {
		t.Errorf("exactly at the window = %v, want held", got)
	}
	clk.advance(time.Nanosecond)
	if got := d.Judge(clk.Now(), lostConditions); got != VerdictLost {
		t.Errorf("one tick past the window = %v, want lost", got)
	}
}

func TestO20ASessionThatIsUpKeepsTheActiveHealthy(t *testing.T) {
	clk := newFakeClock()
	d := NewDetector(20 * time.Second)
	// Pod and node conditions went stale, the session did not.
	c := Conditions{NoSession: false, Unready: true, StandbyUp: true}
	for range 10 {
		if got := d.Judge(clk.Now(), c); got != VerdictHealthy {
			t.Fatalf("verdict = %v, want healthy", got)
		}
		clk.advance(10 * time.Second)
	}
}

func TestO21APodAndNodeThatAreReadyKeepTheActiveHealthy(t *testing.T) {
	clk := newFakeClock()
	d := NewDetector(20 * time.Second)
	c := Conditions{NoSession: true, Unready: false, StandbyUp: true}
	for range 10 {
		if got := d.Judge(clk.Now(), c); got != VerdictHealthy {
			t.Fatalf("verdict = %v, want healthy", got)
		}
		clk.advance(10 * time.Second)
	}
}

func TestO18AConditionThatStopsHoldingRestartsTheWindow(t *testing.T) {
	clk := newFakeClock()
	d := NewDetector(20 * time.Second)
	d.Judge(clk.Now(), lostConditions)
	clk.advance(15 * time.Second)
	// The session came back for one observation.
	d.Judge(clk.Now(), Conditions{Unready: true, StandbyUp: true})
	clk.advance(10 * time.Second)

	if got := d.Judge(clk.Now(), lostConditions); got != VerdictHeld {
		t.Errorf("verdict = %v, want held: the conditions did not hold continuously", got)
	}
}

func TestO22LeadershipRestartsTheWindow(t *testing.T) {
	clk := newFakeClock()
	d := NewDetector(20 * time.Second)
	d.Judge(clk.Now(), lostConditions)
	clk.advance(100 * time.Second)

	d.Reset() // the operator acquired leadership
	if got := d.Judge(clk.Now(), lostConditions); got != VerdictHeld {
		t.Errorf("first observation after leadership = %v, want held", got)
	}
	clk.advance(19 * time.Second)
	if got := d.Judge(clk.Now(), lostConditions); got != VerdictHeld {
		t.Errorf("19s after leadership = %v, want held", got)
	}
}

func TestO23APlannedLossStartsAtOnce(t *testing.T) {
	d := NewDetector(20 * time.Second)
	if got := d.Judge(newFakeClock().Now(), Conditions{Planned: true}); got != VerdictLost {
		t.Errorf("a pod with a deletion timestamp = %v, want lost with no window", got)
	}
}

func TestARebootedActiveIsLostWithoutAWindow(t *testing.T) {
	d := NewDetector(20 * time.Second)
	if got := d.Judge(newFakeClock().Now(), Conditions{Rebooted: true}); got != VerdictLost {
		t.Errorf("a recorded boot that ended = %v, want lost with no window", got)
	}
}

func TestNoStandbyMeansNoCountedLoss(t *testing.T) {
	clk := newFakeClock()
	d := NewDetector(20 * time.Second)
	c := lostConditions
	c.StandbyUp = false
	d.Judge(clk.Now(), c)
	clk.advance(time.Hour)
	if got := d.Judge(clk.Now(), c); got != VerdictHealthy {
		t.Errorf("verdict = %v, want healthy: the third condition does not hold", got)
	}
}

func guestAt(ordinal int, zone string) Guest {
	return Guest{
		Pod: "mds-" + string(rune('0'+ordinal)), Ordinal: ordinal, Zone: zone,
		Ready: true, NodeReady: true, HasSession: true, Role: nfsexportrpc.RoleStandby,
	}
}

func TestO35ACandidateInAnotherZoneWinsOverALowerOrdinal(t *testing.T) {
	got, ok := ChooseTarget([]Guest{guestAt(1, "a"), guestAt(2, "b")}, "a", "")
	if !ok || got.Ordinal != 2 {
		t.Errorf("target = %+v ok=%v, want ordinal 2 in the other zone", got, ok)
	}
}

func TestO36WithoutZonesTheLowestOrdinalWins(t *testing.T) {
	for name, zones := range map[string][2]string{
		"no labels":     {"", ""},
		"same zone":     {"a", "a"},
		"lost has none": {"b", "c"},
	} {
		t.Run(name, func(t *testing.T) {
			lost := "a"
			if name == "no labels" || name == "lost has none" {
				lost = ""
			}
			got, ok := ChooseTarget([]Guest{guestAt(2, zones[1]), guestAt(1, zones[0])}, lost, "")
			if !ok || got.Ordinal != 1 {
				t.Errorf("target = %+v ok=%v, want the lowest ordinal 1", got, ok)
			}
		})
	}
}

func TestO37TheSameObservationChoosesTheSamePod(t *testing.T) {
	guests := []Guest{guestAt(3, "b"), guestAt(1, "a"), guestAt(2, "b")}
	first, _ := ChooseTarget(guests, "a", "")
	for range 5 {
		reversed := []Guest{guests[2], guests[1], guests[0]}
		again, _ := ChooseTarget(reversed, "a", "")
		if again.Pod != first.Pod {
			t.Fatalf("chose %s then %s from the same observation", first.Pod, again.Pod)
		}
	}
}

func TestOnlyAReadyStandbyWithASessionIsACandidate(t *testing.T) {
	ok := guestAt(1, "")
	for name, mutate := range map[string]func(*Guest){
		"no session":  func(g *Guest) { g.HasSession = false },
		"not ready":   func(g *Guest) { g.Ready = false },
		"deleting":    func(g *Guest) { g.Deleting = true },
		"active role": func(g *Guest) { g.Role = nfsexportrpc.RoleActive },
		"unknown":     func(g *Guest) { g.Role = 0 },
		"promoting":   func(g *Guest) { g.Role = nfsexportrpc.RolePromoting },
	} {
		g := ok
		mutate(&g)
		if Candidate(g) {
			t.Errorf("%s: guest is a candidate, want it excluded", name)
		}
		if _, found := ChooseTarget([]Guest{g}, "", ""); found {
			t.Errorf("%s: ChooseTarget chose it", name)
		}
	}
	if !Candidate(ok) {
		t.Error("a Ready standby with a session is not a candidate")
	}
}

func TestAPreferredCandidateWinsAndAnUnfitOneDoesNot(t *testing.T) {
	two := guestAt(2, "a")
	got, ok := ChooseTarget([]Guest{guestAt(1, "a"), two}, "", pod2)
	if !ok || got.Pod != pod2 {
		t.Errorf("target = %+v, want the preferred mds-2", got)
	}
	two.Ready = false
	got, ok = ChooseTarget([]Guest{guestAt(1, "a"), two}, "", pod2)
	if !ok || got.Pod != pod1 {
		t.Errorf("target = %+v, want mds-1 when the preferred is not a candidate", got)
	}
}

func TestO38AReadThatSucceedsOnTheFirstRetryDecides(t *testing.T) {
	clk := newFakeClock()
	calls := 0
	got, err := readWithRetry(context.Background(), clk, func() (int, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("blip")
		}
		return 7, nil
	})
	if err != nil || got != 7 {
		t.Fatalf("got %d, %v, want 7 from the retry", got, err)
	}
	if len(clk.sleeps) != 1 || clk.sleeps[0] != 2*time.Second {
		t.Errorf("sleeps = %v, want one of 2s", clk.sleeps)
	}
}

func TestO39AReadThatFailsEveryTimeReturnsItsError(t *testing.T) {
	clk := newFakeClock()
	calls := 0
	_, err := readWithRetry(context.Background(), clk, func() (int, error) {
		calls++
		return 0, errors.New("down")
	})
	if err == nil {
		t.Fatal("want the error after every retry failed")
	}
	if calls != 4 {
		t.Errorf("calls = %d, want the first try and 3 retries", calls)
	}
	if len(clk.sleeps) != 3 {
		t.Errorf("sleeps = %v, want 3 of 2s", clk.sleeps)
	}
}

func TestO40TheThirdRetryStillDecidesAndNoFourthIsMade(t *testing.T) {
	clk := newFakeClock()
	calls := 0
	got, err := readWithRetry(context.Background(), clk, func() (int, error) {
		calls++
		if calls < 4 {
			return 0, errors.New("blip")
		}
		return 9, nil
	})
	if err != nil || got != 9 {
		t.Errorf("success on the third retry = %d, %v, want 9", got, err)
	}

	calls = 0
	_, err = readWithRetry(context.Background(), clk, func() (int, error) {
		calls++
		return 0, errors.New("blip")
	})
	if err == nil || calls != 4 {
		t.Errorf("failure on the third retry: err=%v calls=%d, want an error after 4 tries", err, calls)
	}
}

func newStatefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: "ns"}}
}

func TestRecordWriteIsOneUpdateOfThreeAnnotations(t *testing.T) {
	sts := newStatefulSet()
	cl := fake.NewClientBuilder().WithObjects(sts).Build()
	store := &KubeStore{Client: cl, Key: client.ObjectKeyFromObject(sts)}
	var cur appsv1.StatefulSet
	_ = cl.Get(context.Background(), store.Key, &cur)

	rec := Record{Active: pod1, Instance: "uid/cid", Epoch: 3}
	if _, err := store.Write(context.Background(), cur.ResourceVersion, rec); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = cl.Get(context.Background(), store.Key, &cur)
	got, err := ParseRecord(cur.Annotations)
	if err != nil || got != rec {
		t.Errorf("record = %+v, %v, want %+v", got, err, rec)
	}
}

func TestO08AWriteOnAStaleVersionConflicts(t *testing.T) {
	sts := newStatefulSet()
	cl := fake.NewClientBuilder().WithObjects(sts).Build()
	store := &KubeStore{Client: cl, Key: client.ObjectKeyFromObject(sts)}
	var cur appsv1.StatefulSet
	_ = cl.Get(context.Background(), store.Key, &cur)
	stale := cur.ResourceVersion

	if _, err := store.Write(context.Background(), stale, Record{Active: pod0, Instance: "a", Epoch: 1}); err != nil {
		t.Fatalf("the first writer: %v", err)
	}
	_, err := store.Write(context.Background(), stale, Record{Active: pod1, Instance: "b", Epoch: 1})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("the second writer on the same version: %v, want ErrConflict", err)
	}
	_ = cl.Get(context.Background(), store.Key, &cur)
	if got, _ := ParseRecord(cur.Annotations); got.Active != pod0 {
		t.Errorf("active = %q, want the first writer's mds-0", got.Active)
	}
}

func TestParseRecordRefusesAnEpochThatIsNotANumber(t *testing.T) {
	if _, err := ParseRecord(map[string]string{AnnotationEpoch: "two"}); err == nil {
		t.Error("want an error for a hand-edited epoch")
	}
	if rec, err := ParseRecord(nil); err != nil || !rec.Empty() || rec.Epoch != 0 {
		t.Errorf("empty annotations = %+v, %v, want the empty record", rec, err)
	}
}

// A guest that is frozen leaves its runner running, so the session stays up and
// the loss shows only as a guest that does not answer while its pod is not Ready.
var hungConditions = Conditions{Unresponsive: true, Unready: true, StandbyUp: true}

func TestAnActiveThatHoldsASessionButDoesNotAnswerIsLostAfterTheWindow(t *testing.T) {
	clk := newFakeClock()
	d := NewDetector(20 * time.Second)

	if got := d.Judge(clk.Now(), hungConditions); got != VerdictHeld {
		t.Fatalf("first observation = %v, want held", got)
	}
	clk.advance(20 * time.Second)
	if got := d.Judge(clk.Now(), hungConditions); got != VerdictHeld {
		t.Errorf("at the window = %v, want held", got)
	}
	clk.advance(time.Second)
	if got := d.Judge(clk.Now(), hungConditions); got != VerdictLost {
		t.Errorf("past the window = %v, want lost", got)
	}
}

func TestAHungActiveNeedsEachOfItsSignalsAndAStandby(t *testing.T) {
	for name, c := range map[string]Conditions{
		"it answers (a guest that is promoting is not Ready yet)": {Unready: true, StandbyUp: true},
		"its pod is Ready (one slow answer)":                      {Unresponsive: true, StandbyUp: true},
		"no standby could take over":                              {Unresponsive: true, Unready: true},
	} {
		clk := newFakeClock()
		d := NewDetector(20 * time.Second)
		d.Judge(clk.Now(), c)
		clk.advance(time.Minute)
		if got := d.Judge(clk.Now(), c); got != VerdictHealthy {
			t.Errorf("%s: verdict = %v, want healthy", name, got)
		}
	}
}
