package devmapper

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recorder struct {
	calls []string
	out   map[string]string
	err   map[string]error
}

func (r *recorder) run(_ context.Context, args ...string) (string, error) {
	key := strings.Join(args, " ")
	r.calls = append(r.calls, key)
	for prefix, err := range r.err {
		if strings.HasPrefix(key, prefix) {
			return r.out[prefix], err
		}
	}
	for prefix, out := range r.out {
		if strings.HasPrefix(key, prefix) {
			return out, nil
		}
	}
	return "", nil
}

func TestTableParsesAWholeDeviceLinearMapping(t *testing.T) {
	r := &recorder{out: map[string]string{"dmsetup table sb-v": "0 2097152 linear 259:3 0\n"}}
	got, err := New(r.run).Table(context.Background(), "sb-v")
	if err != nil || got != (Target{Sectors: 2097152, Device: "259:3"}) {
		t.Fatalf("Table = %+v, %v", got, err)
	}
}

func TestTableOfAMissingMappingIsErrNotFound(t *testing.T) {
	r := &recorder{
		out: map[string]string{"dmsetup table": "device-mapper: table ioctl on sb-v failed: No such device or address"},
		err: map[string]error{"dmsetup table": errors.New("exit status 1")},
	}
	if _, err := New(r.run).Table(context.Background(), "sb-v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTableRefusesAMappingThatIsNotOneLinearSegment(t *testing.T) {
	for _, out := range []string{
		"0 100 striped 2 128 259:3 0 259:4 0",
		"0 100 linear 259:3 0\n100 100 linear 259:4 0",
		"0 100 linear 259:3 8",
	} {
		if _, err := parseTable("sb-v", out); err == nil {
			t.Errorf("accepted %q", out)
		}
	}
}

func TestSwapSuspendsReloadsAndResumes(t *testing.T) {
	r := &recorder{}
	if err := New(r.run).Swap(context.Background(), "sb-v", "259:11", 2048); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"dmsetup suspend --nolockfs sb-v",
		"dmsetup reload sb-v --table 0 2048 linear 259:11 0",
		"dmsetup resume sb-v",
	}
	if strings.Join(r.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", r.calls, want)
	}
}

// Regression: 2026-11-19-dmlinear-heal-freezes-a-dead-filesystem (e2e run
// 44463031746). After total path loss the mapping still points at the removed
// namespace, and a filesystem mounted on it cannot be frozen: ext4's freeze
// fails with EIO, XFS's blocks in xfs_ail_push_all_sync holding the superblock
// lock, and every sync(2) on the node then hangs behind it. The recorder answers
// a freezing suspend the way ext4 did; a swap that freezes never re-points the
// mapping at the namespace the fabric just reconnected.
func TestSwapReachesTheNewDeviceUnderAFilesystemThatCannotFreeze(t *testing.T) {
	r := &recorder{
		out: map[string]string{"dmsetup suspend sb-v": "device-mapper: suspend ioctl on sb-v  failed: Input/output error"},
		err: map[string]error{"dmsetup suspend sb-v": errors.New("exit status 1")},
	}
	if err := New(r.run).Swap(context.Background(), "sb-v", "259:11", 2048); err != nil {
		t.Fatalf("Swap = %v, want the mapping re-pointed", err)
	}
	if last := r.calls[len(r.calls)-1]; last != "dmsetup resume sb-v" {
		t.Fatalf("calls = %v, want a resume onto the new table last", r.calls)
	}
}

// A refused reload must not leave the mapping suspended: I/O queued during the
// suspend would hang until someone resumed it by hand.
func TestAFailedReloadResumesOnTheOldTable(t *testing.T) {
	r := &recorder{err: map[string]error{"dmsetup reload": errors.New("invalid table")}}
	err := New(r.run).Swap(context.Background(), "sb-v", "259:11", 2048)
	if err == nil || !strings.Contains(err.Error(), "resumed on the old table") {
		t.Fatalf("err = %v", err)
	}
	if r.calls[len(r.calls)-1] != "dmsetup resume sb-v" {
		t.Fatalf("last call %q, want a resume", r.calls[len(r.calls)-1])
	}
}

func TestRemovingAMissingMappingSucceeds(t *testing.T) {
	r := &recorder{
		out: map[string]string{"dmsetup remove": "No such device or address"},
		err: map[string]error{"dmsetup remove": errors.New("exit status 1")},
	}
	if err := New(r.run).Remove(context.Background(), "sb-v"); err != nil {
		t.Fatal(err)
	}
}

func TestSectorsReadsBlockdev(t *testing.T) {
	r := &recorder{out: map[string]string{"blockdev --getsz /dev/nvme1n1": "2097152\n"}}
	n, err := New(r.run).Sectors(context.Background(), "/dev/nvme1n1")
	if err != nil || n != 2097152 {
		t.Fatalf("Sectors = %d, %v", n, err)
	}
}

// Suspend and Resume name the mapping and nothing else: a bare suspend is the
// one that freezes a filesystem on top (lockfs), which is what makes the image
// underneath consistent, and --nolockfs would silently drop that.
func TestSuspendAndResumeCommands(t *testing.T) {
	r := &recorder{}
	m := New(r.run)
	if err := m.Suspend(context.Background(), "vol--a-lv--a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(context.Background(), "vol--a-lv--a"); err != nil {
		t.Fatal(err)
	}
	want := []string{"dmsetup suspend vol--a-lv--a", "dmsetup resume vol--a-lv--a"}
	if strings.Join(r.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q, want %q", r.calls, want)
	}
}

func TestSuspendReportsTheFailure(t *testing.T) {
	r := &recorder{err: map[string]error{"dmsetup suspend": errors.New("exit status 1")}}
	if err := New(r.run).Suspend(context.Background(), "x"); err == nil {
		t.Fatal("a failed suspend was reported as success")
	}
}
