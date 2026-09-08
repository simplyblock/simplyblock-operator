// What the stack record has to guarantee.
//
// The record is what makes a partially built stack removable, so the properties
// tested here are about durability and ordering rather than about content: it is
// written before the first side effect, it is never left torn, and a version it
// does not recognize stops a teardown rather than driving one from a plan nobody
// built.

package volstack

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testHandle = "11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222:33333333-3333-3333-3333-333333333333"

func newStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

// A record round-trips: what was written is what a later process reads, in
// order, because Down walks that order backward.
func TestRecordRoundTrip(t *testing.T) {
	s := newStore(t)
	want := Record{
		Version:      RecordVersion,
		VolumeHandle: testHandle,
		Plan: []Entry{
			{Layer: "fabric", Params: json.RawMessage(`{"nqn":"nqn.test:lvol:aaaa"}`)},
			{Layer: "filesystem", Params: json.RawMessage(`{"fsType":"xfs"}`)},
		},
	}
	if err := s.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := s.Load(testHandle)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.VolumeHandle != testHandle {
		t.Errorf("VolumeHandle = %q", got.VolumeHandle)
	}
	if len(got.Plan) != 2 || got.Plan[0].Layer != "fabric" || got.Plan[1].Layer != "filesystem" {
		t.Fatalf("plan did not survive the round trip in order: %+v", got.Plan)
	}
	if string(got.Plan[0].Params) != `{"nqn":"nqn.test:lvol:aaaa"}` {
		t.Errorf("params are not opaque: %s", got.Plan[0].Params)
	}
}

// An absent record means the legacy plan rather than an error, because a volume
// staged by a version that predates the record has no file and still has to be
// unstageable.
func TestLoadAbsentRecordIsNotAnError(t *testing.T) {
	s := newStore(t)
	_, err := s.Load(testHandle)
	if !errors.Is(err, ErrNoRecord) {
		t.Fatalf("Load of an absent record = %v, want ErrNoRecord so the caller can fall back", err)
	}
}

// A version the reader does not recognize refuses the record. A teardown driven
// by a misread plan is worse than one that stops and says why.
func TestUnknownVersionIsRefused(t *testing.T) {
	s := newStore(t)
	raw := []byte(`{"version":9999,"volumeHandle":"` + testHandle + `","plan":[{"layer":"fabric"}]}`)
	if err := os.WriteFile(s.path(testHandle), raw, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := s.Load(testHandle)
	if err == nil {
		t.Fatal("a record from the future was accepted")
	}
	if errors.Is(err, ErrNoRecord) {
		t.Fatal("a record from the future was reported as no record, which would run the legacy plan over it")
	}
}

// A torn file would be read as a plan nobody built, which is worse than no file,
// because no file means the legacy plan. So the write goes somewhere else first
// and is renamed into place.
func TestWriteIsAtomic(t *testing.T) {
	s := newStore(t)
	rec := Record{Version: RecordVersion, VolumeHandle: testHandle, Plan: []Entry{{Layer: "fabric"}}}
	if err := s.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the directory holds %v, want only the record: a temporary file left behind is a torn write waiting to be read", names)
	}
}

// The file outlives the pod that wrote it and its parameters name secrets, so it
// is not world-readable and neither is its directory.
func TestRecordIsNotWorldReadable(t *testing.T) {
	s := newStore(t)
	if err := s.Write(Record{Version: RecordVersion, VolumeHandle: testHandle}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	fi, err := os.Stat(s.path(testHandle))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("record mode = %o, want 600", perm)
	}
	di, err := os.Stat(s.dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %o, want 700", perm)
	}
}

// Marking a layer rewrites the whole record, which is affordable because the
// record holds no device state, and is what makes "what was attempted, in what
// order" answerable after a crash.
func TestMarkAttemptedPersists(t *testing.T) {
	s := newStore(t)
	rec := Record{Version: RecordVersion, VolumeHandle: testHandle,
		Plan: []Entry{{Layer: "fabric"}, {Layer: "filesystem"}}}
	if err := s.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := s.MarkAttempted(testHandle, 0); err != nil {
		t.Fatalf("MarkAttempted: %v", err)
	}

	got, err := s.Load(testHandle)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Plan[0].Attempted {
		t.Error("the marked layer did not survive")
	}
	if got.Plan[1].Attempted {
		t.Error("an unmarked layer came back marked")
	}
}

// Removing a record is idempotent, because a teardown may resume after a crash
// that happened between the last Release and the removal.
func TestRemoveIsIdempotent(t *testing.T) {
	s := newStore(t)
	if err := s.Write(Record{Version: RecordVersion, VolumeHandle: testHandle}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Remove(testHandle); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	if err := s.Remove(testHandle); err != nil {
		t.Fatalf("second Remove: %v, want nil for an already-removed record", err)
	}
}

// A volume handle carries colons and is not a filename. Whatever the store makes
// of it has to stay inside its own directory.
func TestHandleCannotEscapeTheDirectory(t *testing.T) {
	s := newStore(t)
	for _, handle := range []string{
		"../../etc/passwd",
		"a/b/c",
		testHandle,
	} {
		p := s.path(handle)
		if dir := filepath.Dir(p); dir != s.dir {
			t.Errorf("handle %q produced %q, which is not directly in %q", handle, p, s.dir)
		}
		if base := filepath.Base(p); base == "." || base == ".." || strings.ContainsRune(base, filepath.Separator) {
			t.Errorf("handle %q produced the unusable filename %q", handle, base)
		}
	}
}
