// Read-only mappings: a dm-linear device that refuses writes, for a volume
// every consumer only reads (ReadOnlyMany).
//
// The mode belongs to the loaded table, not to the device: dmsetup loads a
// writable table unless told otherwise, so a reload that does not repeat
// --readonly makes a read-only mapping writable again. Every reload here
// therefore reads the mapping's mode first and keeps it.

package devmapper

import (
	"context"
	"fmt"
	"strings"
)

// CreateReadOnly maps name onto device over sectors with a table that refuses
// writes. The kernel then fails every write to the device with EPERM, from any
// opener, including one that opened it before.
func (m *Mapper) CreateReadOnly(ctx context.Context, name, device string, sectors uint64) error {
	if _, err := m.run(ctx, "dmsetup", "create", "--readonly", name, "--table", table(sectors, device)); err != nil {
		return fmt.Errorf("dmsetup create --readonly %s: %w", name, err)
	}
	return nil
}

// ReadOnly reports whether mapping name's live table refuses writes, or
// ErrNotFound. dmsetup prints the attributes as four characters whose last is
// r for a read-only table and w for a writable one (L--r, Ls-w, ...).
func (m *Mapper) ReadOnly(ctx context.Context, name string) (bool, error) {
	out, err := m.run(ctx, "dmsetup", "info", "-c", "--noheadings", "-o", "attr", name)
	if err != nil {
		if notFound(out, err) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("dmsetup info %s: %w", name, err)
	}
	attr := strings.TrimSpace(out)
	if attr == "" {
		return false, fmt.Errorf("dmsetup info %s: no attributes", name)
	}
	return strings.HasSuffix(attr, "r"), nil
}

// MakeReadOnly reloads mapping name's current table read-only: suspend without
// a freeze, load the same table with --readonly, resume. A failed load resumes
// on the old table, so the mapping is never left suspended.
func (m *Mapper) MakeReadOnly(ctx context.Context, name string) error {
	target, err := m.Table(ctx, name)
	if err != nil {
		return err
	}
	if err := m.SuspendWithoutFreeze(ctx, name); err != nil {
		return err
	}
	if _, err := m.run(ctx, "dmsetup", "reload", "--readonly", name,
		"--table", table(target.Sectors, target.Device)); err != nil {
		if rerr := m.Resume(ctx, name); rerr != nil {
			return fmt.Errorf("dmsetup reload --readonly %s: %w; resume also failed: %v", name, err, rerr)
		}
		return fmt.Errorf("dmsetup reload --readonly %s: %w (resumed writable)", name, err)
	}
	return m.Resume(ctx, name)
}

// reload loads tableText into mapping name in the mode its live table has.
func (m *Mapper) reload(ctx context.Context, name, tableText string) error {
	readOnly, err := m.ReadOnly(ctx, name)
	if err != nil {
		return fmt.Errorf("read the mode of %s: %w", name, err)
	}
	args := []string{"dmsetup", "reload"}
	if readOnly {
		args = append(args, "--readonly")
	}
	_, err = m.run(ctx, append(args, name, "--table", tableText)...)
	return err
}
