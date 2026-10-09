// A watchdog over the guest's nfsd, read from /proc and written to the agent's
// log. journald passes that log to the console, which the runner streams into
// the pod log.
//
// It lives in the agent because the agent is the only process in the guest that
// runs all the time and logs. In run pnfs-1791558094 nfsd stopped closing its
// client connections for good while the guest kernel kept running, and nothing
// in the console said why. The watchdog logs a periodic summary of nfsd's
// threads and pool counters, and when a thread waits on anything but new work
// for several ticks running, or work arrives that no thread wakes for, it warns
// once and logs the kernel stack of every nfsd thread. It only reads /proc: it
// never signals, kills, or restarts anything.

package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog"
)

const (
	// DefaultNFSDWatchdogInterval is the time between two looks at nfsd.
	DefaultNFSDWatchdogInterval = 15 * time.Second

	// summaryEvery is how many ticks one summary line covers, a minute at the
	// default interval.
	summaryEvery = 4
	// stuckTicks is how many consecutive ticks a thread waits on the same
	// non-idle wchan, or work arrives without a thread waking, before it counts.
	stuckTicks = 3
	// stackDumpEvery limits the stack dumps of one episode.
	stackDumpEvery = 5 * time.Minute
)

// idleWaits are the wchans of an nfsd thread waiting for its next request. The
// name of that wait has changed across kernel releases, so a wchan containing
// any of these is idle, as is none at all.
var idleWaits = []string{"svc_recv", "svc_get_next_xprt", "svc_thread_wait"}

func isIdleWait(wchan string) bool {
	if wchan == "" || wchan == "0" {
		return true
	}
	for _, idle := range idleWaits {
		if strings.Contains(wchan, idle) {
			return true
		}
	}
	return false
}

// Logger is where the watchdog writes. klog's package functions in the agent,
// a recorder in tests.
type Logger interface {
	Infof(format string, args ...any)
	Warningf(format string, args ...any)
}

// KlogLogger writes to klog.
type KlogLogger struct{}

// Infof logs at info level.
func (KlogLogger) Infof(format string, args ...any) { klog.Infof(format, args...) }

// Warningf logs at warning level.
func (KlogLogger) Warningf(format string, args ...any) { klog.Warningf(format, args...) }

// poolStats are nfsd's pool counters summed over its pools.
type poolStats struct{ Arrived, Enqueued, Woken, TimedOut uint64 }

func (p poolStats) minus(o poolStats) poolStats {
	return poolStats{Arrived: p.Arrived - o.Arrived, Enqueued: p.Enqueued - o.Enqueued,
		Woken: p.Woken - o.Woken, TimedOut: p.TimedOut - o.TimedOut}
}

// readPoolStats reads <root>/fs/nfsd/pool_stats. Not ok, and no error, when the
// file is absent, which is nfsd not running. The columns are found by their
// names in the header line, which is how the file describes itself.
func readPoolStats(root string) (poolStats, bool, error) {
	f, err := os.Open(filepath.Join(root, "fs", "nfsd", "pool_stats"))
	if errors.Is(err, fs.ErrNotExist) {
		return poolStats{}, false, nil
	}
	if err != nil {
		return poolStats{}, false, err
	}
	defer f.Close() //nolint:errcheck // read-only

	var columns []string
	var sum poolStats
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "#" {
			columns = fields[1:]
			continue
		}
		for i, value := range fields {
			if i >= len(columns) {
				break
			}
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				continue
			}
			switch columns[i] {
			case "packets-arrived":
				sum.Arrived += n
			case "sockets-enqueued":
				sum.Enqueued += n
			case "threads-woken":
				sum.Woken += n
			case "threads-timedout":
				sum.TimedOut += n
			}
		}
	}
	return sum, true, scanner.Err()
}

// nfsdThread is one nfsd kernel thread as /proc showed it.
// readRPCCalls is the count of requests nfsd has processed, the first field of
// the rpc line in <root>/net/rpc/nfsd. It moves only when a request completes,
// which is the progress the backlog rule compares arrivals with: threads-woken
// counts idle threads woken, and busy threads drain a backlog without waking
// anyone. ok is false when the file or the line is absent.
func readRPCCalls(root string) (calls uint64, ok bool) {
	data, err := os.ReadFile(filepath.Join(root, "net", "rpc", "nfsd"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "rpc" {
			n, err := strconv.ParseUint(fields[1], 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

type nfsdThread struct {
	PID   int
	State string
	WChan string
}

// stateFromStat is the state field of a /proc/<pid>/stat line. It follows the
// command name, which sits in parentheses and may hold spaces and parentheses
// itself, so the state is the first field after the last closing one.
func stateFromStat(stat string) string {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// readNFSDThreads lists the threads under root whose command name is nfsd, in
// pid order. A thread that exits while it is read is skipped.
func readNFSDThreads(root string) ([]nfsdThread, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var threads []nfsdThread
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		comm, err := os.ReadFile(filepath.Join(dir, "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "nfsd" {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		wchan, _ := os.ReadFile(filepath.Join(dir, "wchan"))
		threads = append(threads, nfsdThread{
			PID: pid, State: stateFromStat(string(stat)), WChan: strings.TrimSpace(string(wchan)),
		})
	}
	sort.Slice(threads, func(i, j int) bool { return threads[i].PID < threads[j].PID })
	return threads, nil
}

// waiting is how long one thread has waited on one wchan.
type waiting struct {
	wchan string
	ticks int
}

// NFSDWatchdog looks at nfsd once per Tick. It is not safe for concurrent use:
// RunNFSDWatchdog ticks it from one goroutine.
type NFSDWatchdog struct {
	// ProcRoot is where /proc is, so a test can point it at a directory.
	ProcRoot string
	// Log receives the summaries, the warnings, and the stacks.
	Log Logger
	// The clock, which a test sets. Unset means time.Now.
	Now func() time.Time

	ticks       int
	waits       map[int]waiting
	pools       []poolStats // the last stuckTicks+1 readings, oldest first
	calls       []uint64    // processed RPC calls at the same ticks, oldest first
	lastSummary *poolStats
	inEpisode   bool
	episodeAt   time.Time
	lastDump    time.Time
}

func (w *NFSDWatchdog) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Tick reads nfsd's state once, logs the summary when one is due, and starts,
// continues, or ends a stuck episode.
func (w *NFSDWatchdog) Tick() {
	root := w.ProcRoot
	if root == "" {
		root = "/proc"
	}
	w.ticks++
	pool, running, err := readPoolStats(root)
	if err != nil {
		w.Log.Infof("nfsd watchdog: reading pool_stats: %v", err)
	}
	threads, err := readNFSDThreads(root)
	if err != nil {
		w.Log.Infof("nfsd watchdog: listing threads: %v", err)
	}

	stuck := w.trackWaits(threads)
	calls, haveCalls := readRPCCalls(root)
	notDraining := running && w.trackPool(pool, calls, haveCalls)
	if running && w.ticks%summaryEvery == 0 {
		w.summarize(threads, pool)
	}

	switch {
	case (len(stuck) > 0 || notDraining) && !w.inEpisode:
		w.inEpisode, w.episodeAt = true, w.now()
		w.Log.Warningf("nfsd watchdog: nfsd looks stuck: %s", describeEpisode(stuck, notDraining))
		w.dumpStacks(root, threads)
	case (len(stuck) > 0 || notDraining) && w.now().Sub(w.lastDump) >= stackDumpEvery:
		w.Log.Infof("nfsd watchdog: still stuck after %s: %s",
			w.now().Sub(w.episodeAt).Round(time.Second), describeEpisode(stuck, notDraining))
		w.dumpStacks(root, threads)
	case len(stuck) == 0 && !notDraining && w.inEpisode:
		w.inEpisode = false
		w.Log.Infof("nfsd watchdog: episode ended after %s", w.now().Sub(w.episodeAt).Round(time.Second))
	}
}

// trackWaits counts, per thread, the consecutive ticks on one non-idle wchan,
// and returns the threads that reached stuckTicks.
func (w *NFSDWatchdog) trackWaits(threads []nfsdThread) []nfsdThread {
	next := make(map[int]waiting, len(threads))
	var stuck []nfsdThread
	for _, t := range threads {
		if isIdleWait(t.WChan) {
			continue
		}
		wait := waiting{wchan: t.WChan, ticks: 1}
		if prev, ok := w.waits[t.PID]; ok && prev.wchan == t.WChan {
			wait.ticks = prev.ticks + 1
		}
		next[t.PID] = wait
		if wait.ticks >= stuckTicks {
			stuck = append(stuck, t)
		}
	}
	w.waits = next
	return stuck
}

// trackPool reports whether work arrived over the last stuckTicks ticks while
// nfsd processed no request. Without the RPC statistics it does not judge:
// there is no progress to compare the arrivals with.
func (w *NFSDWatchdog) trackPool(pool poolStats, calls uint64, haveCalls bool) bool {
	if !haveCalls {
		w.pools, w.calls = nil, nil
		return false
	}
	w.pools = append(w.pools, pool)
	w.calls = append(w.calls, calls)
	if len(w.pools) > stuckTicks+1 {
		w.pools = w.pools[len(w.pools)-stuckTicks-1:]
		w.calls = w.calls[len(w.calls)-stuckTicks-1:]
	}
	if len(w.pools) <= stuckTicks {
		return false
	}
	moved := pool.minus(w.pools[0])
	return (moved.Arrived > 0 || moved.Enqueued > 0) && calls == w.calls[0]
}

// summarize logs one line: threads by state, the wchans they wait in, and the
// pool counters' movement since the last summary.
func (w *NFSDWatchdog) summarize(threads []nfsdThread, pool poolStats) {
	states := map[string]int{}
	wchans := map[string]int{}
	for _, t := range threads {
		states[t.State]++
		wchan := t.WChan
		if wchan == "" {
			wchan = "-"
		}
		wchans[wchan]++
	}
	moved := "first reading"
	if w.lastSummary != nil {
		d := pool.minus(*w.lastSummary)
		moved = fmt.Sprintf("arrived +%d enqueued +%d woken +%d timedout +%d",
			d.Arrived, d.Enqueued, d.Woken, d.TimedOut)
	}
	w.lastSummary = &pool
	w.Log.Infof("nfsd: %d thread(s) [%s] waiting in [%s]; %s",
		len(threads), joinCounts(states), joinCounts(wchans), moved)
}

// dumpStacks logs the kernel stack of every nfsd thread.
func (w *NFSDWatchdog) dumpStacks(root string, threads []nfsdThread) {
	w.lastDump = w.now()
	for _, t := range threads {
		stack, err := os.ReadFile(filepath.Join(root, strconv.Itoa(t.PID), "stack"))
		if err != nil {
			w.Log.Infof("nfsd watchdog: stack of nfsd thread %d (%s, %s): unreadable: %v",
				t.PID, t.State, t.WChan, err)
			continue
		}
		w.Log.Infof("nfsd watchdog: stack of nfsd thread %d (%s, %s):\n%s",
			t.PID, t.State, t.WChan, strings.TrimRight(string(stack), "\n"))
	}
}

func describeEpisode(stuck []nfsdThread, notDraining bool) string {
	var parts []string
	for _, t := range stuck {
		parts = append(parts, fmt.Sprintf("pid %d %s in %s for %d ticks", t.PID, t.State, t.WChan, stuckTicks))
	}
	if notDraining {
		parts = append(parts, fmt.Sprintf(
			"not draining: work arrived over %d ticks and no request was processed", stuckTicks))
	}
	return strings.Join(parts, "; ")
}

func joinCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, " ")
}

// RunNFSDWatchdog ticks w every interval until ctx ends. An interval of zero or
// less turns it off.
func RunNFSDWatchdog(ctx context.Context, w *NFSDWatchdog, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick()
		}
	}
}
