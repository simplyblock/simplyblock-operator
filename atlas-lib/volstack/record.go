// The stack record: what was planned for a volume, and how far bringing it up
// got.
//
// It is written before the first side effect and removed after the last release,
// which is what makes a partially built stack removable. A crash between a
// fabric connect and the recording of that connect would otherwise leave paths
// attached that nothing ever releases.

package volstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RecordVersion is the schema version this package writes and the only one it
// reads.
const RecordVersion = 1

// ErrNoRecord reports that a volume has no record. It is not a failure: a volume
// staged by a version predating this design has none, and unstaging it uses the
// legacy plan.
var ErrNoRecord = errors.New("volstack: no stack record for this volume")

// Record is the on-disk form of a stack, one file per volume.
type Record struct {
	// Version is the schema version of this file and not the release that wrote
	// it. A reader that does not recognize it refuses the record rather than
	// guessing, because a teardown driven by a misread plan is worse than a
	// teardown that stops and says why.
	Version int `json:"version"`

	// VolumeHandle repeats the filename so that a record found on its own
	// identifies itself.
	VolumeHandle string `json:"volumeHandle"`

	// Plan is the ordered layer list, bottom first. The order is most of why the
	// file exists: Up walks it forward and Down walks it back.
	Plan []Entry `json:"plan"`
}

// Entry is one layer as the plan named it.
type Entry struct {
	// Layer is the value Layer.Name() returns, stable across releases.
	Layer string `json:"layer"`

	// Params is what the layer was constructed with, opaque to the runner: the
	// layer that declared them is the only thing that parses them. A new layer
	// therefore ships without this format changing.
	Params json.RawMessage `json:"params,omitempty"`

	// Members is the ordered sub-plan of a fan-in layer and is empty for every
	// other layer. It is a field of its own rather than part of Params because
	// the runner walks it, and member order is a runner concern.
	Members []Entry `json:"members,omitempty"`

	// Attempted records that Ensure was called on this layer. It is a diagnostic
	// and an optimization, never a correctness mechanism.
	Attempted bool `json:"attempted"`
}

// Store is the directory of stack records on one host.
type Store struct {
	dir string
}

// NewStore returns a store over dir, which has to outlive the container: a
// plugin restart is an ordinary event and the record is the only thing that
// tells the restarted process what the previous one built.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// path is the record's file, named for the volume handle with the separators a
// handle carries made safe for a filename. It never leaves the store's own
// directory, whatever it is handed.
func (s *Store) path(handle string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '_'
		}
	}, handle)
	if safe == "" || strings.Trim(safe, ".") == "" {
		safe = "_"
	}
	return filepath.Join(s.dir, safe+".json")
}

// Write persists rec.
//
// The write goes to a temporary file in the same directory, is fsynced, is
// renamed over the target, and the directory is fsynced after the rename. A torn
// file would be worse than no file at all, because an absent record means the
// legacy plan and a half-written one would be read as a plan nobody built.
func (s *Store) Write(rec Record) error {
	if rec.Version == 0 {
		rec.Version = RecordVersion
	}
	// Marshal rather than MarshalIndent: an indenting encoder rewrites the
	// embedded raw parameters, and those are the layer's bytes rather than the
	// runner's to reformat. The file is machine-written and machine-read.
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("volstack: encode the record for %s: %w", rec.VolumeHandle, err)
	}

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("volstack: create %s: %w", s.dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, and the records name
	// secrets, so the mode is asserted rather than assumed.
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return fmt.Errorf("volstack: restrict %s: %w", s.dir, err)
	}

	tmp, err := os.CreateTemp(s.dir, ".record-*.tmp")
	if err != nil {
		return fmt.Errorf("volstack: create a temporary record in %s: %w", s.dir, err)
	}
	tmpName := tmp.Name()
	// Every path below this point removes the temporary file, because one left
	// behind is a torn write waiting to be mistaken for a record.
	defer func() { _ = os.Remove(tmpName) }()

	if err := writeAndSync(tmp, data); err != nil {
		return fmt.Errorf("volstack: write the record for %s: %w", rec.VolumeHandle, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("volstack: set the mode of the record for %s: %w", rec.VolumeHandle, err)
	}
	if err := os.Rename(tmpName, s.path(rec.VolumeHandle)); err != nil {
		return fmt.Errorf("volstack: install the record for %s: %w", rec.VolumeHandle, err)
	}
	return syncDir(s.dir)
}

// writeAndSync writes data and forces it to the medium before the file is
// closed, so the rename that follows installs a complete file rather than a
// name pointing at bytes that never landed.
func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// syncDir forces the rename itself to the medium. Without it the file survives a
// crash and its name may not.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("volstack: open %s to sync it: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		// A directory that cannot be synced is not a reason to fail a write that
		// already landed, and some filesystems refuse it outright.
		return nil
	}
	return nil
}

// Load reads a volume's record, reporting ErrNoRecord when it has none.
func (s *Store) Load(handle string) (Record, error) {
	data, err := os.ReadFile(s.path(handle))
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, fmt.Errorf("%s: %w", handle, ErrNoRecord)
	}
	if err != nil {
		return Record{}, fmt.Errorf("volstack: read the record for %s: %w", handle, err)
	}

	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("volstack: the record for %s is not readable: %w", handle, err)
	}
	if rec.Version != RecordVersion {
		return Record{}, fmt.Errorf(
			"volstack: the record for %s is version %d and this build reads version %d, refusing to drive a teardown from a plan it may be misreading",
			handle, rec.Version, RecordVersion)
	}
	return rec, nil
}

// MarkAttempted records that Ensure was called on the layer at index i.
//
// It rewrites the whole record, which is one small local write per layer and is
// affordable precisely because the record holds no device state.
func (s *Store) MarkAttempted(handle string, i int) error {
	rec, err := s.Load(handle)
	if err != nil {
		return err
	}
	if i < 0 || i >= len(rec.Plan) {
		return fmt.Errorf("volstack: layer %d is outside the recorded plan for %s, which has %d",
			i, handle, len(rec.Plan))
	}
	rec.Plan[i].Attempted = true
	return s.Write(rec)
}

// Remove deletes a volume's record. It is idempotent, because a teardown may
// resume after a crash between the last Release and this.
func (s *Store) Remove(handle string) error {
	err := os.Remove(s.path(handle))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("volstack: remove the record for %s: %w", handle, err)
	}
	return nil
}
