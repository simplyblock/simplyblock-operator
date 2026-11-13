package node

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/freeze"
)

// recordingMapper logs suspends and resumes instead of running dmsetup.
type recordingMapper struct {
	mu    sync.Mutex
	calls []string
}

func (m *recordingMapper) Suspend(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "suspend "+name)
	return nil
}

func (m *recordingMapper) Resume(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "resume "+name)
	return nil
}

func freezingServer(t *testing.T) (*Server, *recordingMapper) {
	t.Helper()
	ns, _ := newStackedServer(t, newRecordingRunner())
	m := &recordingMapper{}
	ns.freezes = freeze.New(freeze.Config{Mapper: m})
	return ns, m
}

// The device a striped volume is frozen through is its logical volume's
// device-mapper node, named after the first member the way the plan named it.
func TestFreezeSuspendsTheStripesLogicalVolume(t *testing.T) {
	ns, m := freezingServer(t)
	handle := pvcTestHandle + ":s3"
	writeStripedRecord(t, ns.stack, handle, []string{pvcTestLvol, memberB, memberC}, true)

	frozen, err := ns.Freeze(context.Background(), handle, "tok", 5*time.Second)
	if err != nil || !frozen {
		t.Fatalf("Freeze = %v, %v", frozen, err)
	}
	escaped := strings.ReplaceAll(pvcTestLvol, "-", "--")
	device := "vol--" + escaped + "-lv--" + escaped
	held, err := ns.Thaw(context.Background(), handle, "tok")
	if err != nil || !held {
		t.Fatalf("Thaw = %v, %v", held, err)
	}
	if want := []string{"suspend " + device, "resume " + device}; !slices.Equal(m.calls, want) {
		t.Errorf("calls = %v, want %v", m.calls, want)
	}
}

// While a volume is frozen nothing else on the node may act on it: stage,
// unstage, publish, and expand take the same lock, and would otherwise wait on
// the suspended device or change what is under the snapshot.
func TestAFrozenVolumeHoldsItsLock(t *testing.T) {
	ns, _ := freezingServer(t)
	handle := pvcTestHandle + ":s3"
	writeStripedRecord(t, ns.stack, handle, []string{pvcTestLvol, memberB, memberC}, true)
	if _, err := ns.Freeze(context.Background(), handle, "tok", 5*time.Second); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := lockWithin(ctx, ns.volumeLocks, handle); err == nil {
		t.Fatal("the volume's lock was free while it was frozen")
	}
	if _, err := ns.Thaw(context.Background(), handle, "tok"); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockWithin(context.Background(), ns.volumeLocks, handle)
	if err != nil {
		t.Fatalf("the lock stayed taken after the thaw: %v", err)
	}
	unlock()
}

// A volume this node does not stage has no writer here: nothing is suspended.
func TestFreezeOfAVolumeNotStagedHereFreezesNothing(t *testing.T) {
	ns, m := freezingServer(t)
	frozen, err := ns.Freeze(context.Background(), pvcTestHandle+":s3", "tok", time.Second)
	if err != nil || frozen || len(m.calls) != 0 {
		t.Errorf("Freeze = %v, %v with %v; want nothing frozen", frozen, err, m.calls)
	}
}

func TestFreezeRefusesAVolumeThatIsNotStriped(t *testing.T) {
	ns, m := freezingServer(t)
	writeRecord(t, ns.stack, pvcTestHandle, []string{"fabric", "filesystem"})
	_, err := ns.Freeze(context.Background(), pvcTestHandle, "tok", time.Second)
	if status.Code(err) != codes.FailedPrecondition || len(m.calls) != 0 {
		t.Errorf("err = %v with %v, want FailedPrecondition and nothing suspended", err, m.calls)
	}
}

// Another snapshot's freeze is not touched by this one's calls.
func TestFreezeTokensAreTheNodesAnswer(t *testing.T) {
	ns, m := freezingServer(t)
	handle := pvcTestHandle + ":s3"
	writeStripedRecord(t, ns.stack, handle, []string{pvcTestLvol, memberB, memberC}, true)
	if _, err := ns.Freeze(context.Background(), handle, "mine", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := ns.Thaw(context.Background(), handle, "theirs"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Thaw under another token = %v, want FailedPrecondition", err)
	}
	if len(m.calls) != 1 {
		t.Errorf("calls = %v, want the one suspend", m.calls)
	}
	if held, _ := ns.Thaw(context.Background(), handle, "mine"); !held {
		t.Error("the holder's thaw did not find its freeze")
	}
}
