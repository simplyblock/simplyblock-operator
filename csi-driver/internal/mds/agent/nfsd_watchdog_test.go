package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeProc is a /proc tree on a temporary directory: the pool statistics and
// one directory per thread, holding what the watchdog reads.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) *fakeProc {
	t.Helper()
	return &fakeProc{t: t, root: t.TempDir()}
}

func (p *fakeProc) write(rel, content string) {
	p.t.Helper()
	path := filepath.Join(p.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p *fakeProc) pool(arrived, enqueued, woken int) {
	p.write("fs/nfsd/pool_stats", fmt.Sprintf(
		"# pool packets-arrived sockets-enqueued threads-woken threads-timedout\n0 %d %d %d 0\n",
		arrived, enqueued, woken))
}

func (p *fakeProc) thread(pid int, comm, state, wchan string) {
	p.write(fmt.Sprintf("%d/comm", pid), comm+"\n")
	p.write(fmt.Sprintf("%d/stat", pid), fmt.Sprintf("%d (%s) %s 2 0 0 0 -1 0", pid, comm, state))
	p.write(fmt.Sprintf("%d/wchan", pid), wchan)
	p.write(fmt.Sprintf("%d/stack", pid), fmt.Sprintf("[<0>] %s+0x1/0x2\n[<0>] kthread+0x3/0x4\n", wchan))
}

// recorder keeps what the watchdog logs, by level.
type recorder struct{ infos, warnings []string }

func (r *recorder) Infof(format string, args ...any) {
	r.infos = append(r.infos, fmt.Sprintf(format, args...))
}

func (r *recorder) Warningf(format string, args ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, args...))
}

func (r *recorder) all() string {
	return strings.Join(append(append([]string{}, r.infos...), r.warnings...), "\n")
}

func newWatchdog(p *fakeProc, rec *recorder, now *time.Time) *NFSDWatchdog {
	return &NFSDWatchdog{ProcRoot: p.root, Log: rec, Now: func() time.Time { return *now }}
}

func TestPoolStatsAreSummedByColumnName(t *testing.T) {
	p := newFakeProc(t)
	p.write("fs/nfsd/pool_stats",
		"# pool packets-arrived sockets-enqueued threads-woken threads-timedout\n0 10 4 6 0\n1 5 1 3 1\n")
	got, ok, err := readPoolStats(p.root)
	if err != nil || !ok {
		t.Fatalf("readPoolStats = %+v, %v, %v", got, ok, err)
	}
	want := poolStats{Arrived: 15, Enqueued: 5, Woken: 9, TimedOut: 1}
	if got != want {
		t.Errorf("readPoolStats = %+v, want %+v", got, want)
	}
}

func TestMissingPoolStatsIsNFSDNotRunning(t *testing.T) {
	_, ok, err := readPoolStats(newFakeProc(t).root)
	if err != nil || ok {
		t.Errorf("readPoolStats on a guest without nfsd = ok %v, err %v; want not ok and no error", ok, err)
	}
}

// The command name sits in parentheses and may itself hold spaces and
// parentheses, so the state is the field after the last closing one.
func TestThreadStateIsReadAfterTheLastParenthesis(t *testing.T) {
	for stat, want := range map[string]string{
		"412 (nfsd) D 2 0 0":          "D",
		"77 (odd (name) x) S 1 0 0":   "S",
		"9 (a) b c) R 1":              "R",
		"broken line without a paren": "",
		"5 (unterminated":             "",
		"6 (nothing after the paren)": "",
	} {
		if got := stateFromStat(stat); got != want {
			t.Errorf("stateFromStat(%q) = %q, want %q", stat, got, want)
		}
	}
}

func TestOnlyNFSDThreadsAreRead(t *testing.T) {
	p := newFakeProc(t)
	p.thread(100, "nfsd", "S", "svc_recv")
	p.thread(101, "nfsd", "D", "xfs_ilock")
	p.thread(200, "systemd", "S", "do_epoll_wait")
	p.write("self/comm", "mds-agent\n")
	threads, err := readNFSDThreads(p.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 || threads[0].PID != 100 || threads[1].PID != 101 ||
		threads[1].State != "D" || threads[1].WChan != "xfs_ilock" {
		t.Errorf("readNFSDThreads = %+v, want pids 100 and 101 with their state and wchan", threads)
	}
}

// A thread waiting for work is idle however long it waits. One that waits on
// anything else for three ticks running is stuck, and that starts an episode:
// one warning naming it, and the stacks of every nfsd thread.
func TestAThreadWaitingOnTheSameThingForThreeTicksIsStuck(t *testing.T) {
	p := newFakeProc(t)
	p.pool(10, 10, 10)
	p.thread(100, "nfsd", "S", "svc_recv")
	p.thread(101, "nfsd", "D", "nfsd4_cb_sequence_done")
	rec := &recorder{}
	now := time.Unix(0, 0)
	w := newWatchdog(p, rec, &now)

	w.Tick()
	w.Tick()
	if len(rec.warnings) != 0 {
		t.Fatalf("warned after two ticks: %v", rec.warnings)
	}
	w.Tick()

	if len(rec.warnings) != 1 || !strings.Contains(rec.warnings[0], "101") ||
		!strings.Contains(rec.warnings[0], "nfsd4_cb_sequence_done") {
		t.Fatalf("warnings = %v, want one naming pid 101 and its wchan", rec.warnings)
	}
	if strings.Contains(rec.warnings[0], "pid 100 ") {
		t.Errorf("the idle thread was reported stuck: %v", rec.warnings[0])
	}
	all := rec.all()
	for _, want := range []string{"100", "101", "kthread"} {
		if !strings.Contains(all, want) {
			t.Errorf("logs do not carry the stacks of every nfsd thread (missing %q):\n%s", want, all)
		}
	}
}

func TestIdleWaitsAreNeverStuck(t *testing.T) {
	p := newFakeProc(t)
	p.pool(10, 10, 10)
	for i, wchan := range []string{"svc_recv", "svc_get_next_xprt", "svc_thread_wait_for_work", "", "0"} {
		p.thread(100+i, "nfsd", "S", wchan)
	}
	rec := &recorder{}
	now := time.Unix(0, 0)
	w := newWatchdog(p, rec, &now)
	for range 6 {
		w.Tick()
	}
	if len(rec.warnings) != 0 {
		t.Errorf("idle threads raised warnings: %v", rec.warnings)
	}
}

// A wchan that changes is a thread making progress, so its count starts over.
func TestAThreadWhoseWaitChangesIsNotStuck(t *testing.T) {
	p := newFakeProc(t)
	p.pool(10, 10, 10)
	rec := &recorder{}
	now := time.Unix(0, 0)
	w := newWatchdog(p, rec, &now)
	for _, wchan := range []string{"xfs_ilock", "nfsd_file_acquire", "xfs_ilock", "nfsd_file_acquire"} {
		p.thread(101, "nfsd", "D", wchan)
		w.Tick()
	}
	if len(rec.warnings) != 0 {
		t.Errorf("a thread making progress was reported stuck: %v", rec.warnings)
	}
}

// Work arriving while no thread wakes for it is nfsd not draining, even when
// every thread looks idle: the CLOSE_WAIT sockets of pnfs-1791558094.
func TestWorkArrivingWithNoThreadWokenIsNotDraining(t *testing.T) {
	p := newFakeProc(t)
	p.thread(100, "nfsd", "S", "svc_recv")
	rec := &recorder{}
	now := time.Unix(0, 0)
	w := newWatchdog(p, rec, &now)
	for i := range 4 {
		p.pool(10+i, 10+i, 10)
		w.Tick()
	}
	if len(rec.warnings) != 1 || !strings.Contains(rec.warnings[0], "not draining") {
		t.Errorf("warnings = %v, want one saying nfsd is not draining", rec.warnings)
	}
}

func TestWorkThatWakesThreadsIsDraining(t *testing.T) {
	p := newFakeProc(t)
	p.thread(100, "nfsd", "S", "svc_recv")
	rec := &recorder{}
	now := time.Unix(0, 0)
	w := newWatchdog(p, rec, &now)
	for i := range 6 {
		p.pool(10+i, 10+i, 10+i)
		w.Tick()
	}
	if len(rec.warnings) != 0 {
		t.Errorf("a draining nfsd raised warnings: %v", rec.warnings)
	}
}

// One episode dumps the stacks once, and again only after five minutes.
func TestStacksAreDumpedAtMostEveryFiveMinutesInAnEpisode(t *testing.T) {
	p := newFakeProc(t)
	p.pool(10, 10, 10)
	p.thread(101, "nfsd", "D", "xfs_ilock")
	rec := &recorder{}
	now := time.Unix(0, 0)
	w := newWatchdog(p, rec, &now)

	dumps := func() int {
		n := 0
		for _, line := range append(append([]string{}, rec.infos...), rec.warnings...) {
			if strings.Contains(line, "stack of nfsd thread") {
				n++
			}
		}
		return n
	}
	for range 3 {
		w.Tick()
		now = now.Add(15 * time.Second)
	}
	if dumps() != 1 {
		t.Fatalf("dumps after the episode began = %d, want 1", dumps())
	}
	for range 10 { // 150 s more
		w.Tick()
		now = now.Add(15 * time.Second)
	}
	if dumps() != 1 {
		t.Errorf("dumps within five minutes = %d, want still 1", dumps())
	}
	now = now.Add(5 * time.Minute)
	w.Tick()
	if dumps() != 2 {
		t.Errorf("dumps after five minutes = %d, want 2", dumps())
	}
	if len(rec.warnings) != 1 {
		t.Errorf("warnings = %d, want the episode's one warning", len(rec.warnings))
	}
}

func TestTheEndOfAnEpisodeIsLogged(t *testing.T) {
	p := newFakeProc(t)
	p.pool(10, 10, 10)
	p.thread(101, "nfsd", "D", "xfs_ilock")
	rec := &recorder{}
	now := time.Unix(0, 0)
	w := newWatchdog(p, rec, &now)
	for range 3 {
		w.Tick()
	}
	p.thread(101, "nfsd", "S", "svc_recv")
	w.Tick()
	if !strings.Contains(strings.Join(rec.infos, "\n"), "episode ended") {
		t.Errorf("no end of episode logged: %v", rec.infos)
	}
	// A second episode warns again.
	p.thread(101, "nfsd", "D", "xfs_ilock")
	for range 3 {
		w.Tick()
	}
	if len(rec.warnings) != 2 {
		t.Errorf("warnings = %v, want one per episode", rec.warnings)
	}
}

// Every fourth tick logs one line: threads by state, the wchans they wait in,
// and how the pool counters moved since the last summary.
func TestASummaryIsLoggedEveryFourthTick(t *testing.T) {
	p := newFakeProc(t)
	p.thread(100, "nfsd", "S", "svc_recv")
	p.thread(101, "nfsd", "S", "svc_recv")
	rec := &recorder{}
	now := time.Unix(0, 0)
	w := newWatchdog(p, rec, &now)
	for i := range 8 {
		p.pool(10*(i+1), 10*(i+1), 10*(i+1))
		w.Tick()
	}
	var summaries []string
	for _, line := range rec.infos {
		if strings.HasPrefix(line, "nfsd:") {
			summaries = append(summaries, line)
		}
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %v, want two in eight ticks", summaries)
	}
	for _, want := range []string{"2 thread", "S=2", "svc_recv=2", "arrived +40"} {
		if !strings.Contains(summaries[1], want) {
			t.Errorf("summary %q lacks %q", summaries[1], want)
		}
	}
}

func TestAGuestWithoutNFSDLogsNothingAlarming(t *testing.T) {
	p := newFakeProc(t)
	rec := &recorder{}
	now := time.Unix(0, 0)
	w := newWatchdog(p, rec, &now)
	for range 8 {
		w.Tick()
	}
	if len(rec.warnings) != 0 {
		t.Errorf("warnings without nfsd: %v", rec.warnings)
	}
}
