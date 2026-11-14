// Package freeze holds a device-mapper device suspended for a bounded time,
// under a token, so that a caller elsewhere can take several backend snapshots
// of what is underneath it at one point in its write stream.
//
// The case it exists for is a striped volume (csi-driver/docs/striped-volumes.md):
// one logical volume striped across several simplyblock volumes on different
// storage nodes. The control plane snapshots each member separately, and no
// backend primitive snapshots volumes on different nodes atomically, because a
// consistency group is confined to one logical volume store. Suspending the
// stripe on the host that writes it does what the backend cannot: device-mapper
// drains the I/O in flight, queues new I/O, and freezes a filesystem mounted on
// top (dmsetup's default lockfs), so every member holds the same moment until
// the device is resumed.
//
// A suspended device is a stalled application, so the hold is bounded three
// ways. It ends when its holder thaws it. It ends by itself at a deadline the
// holder chose, capped by the host, whether or not the holder is still there.
// And it is recorded on disk while it lasts, so a process that restarts in the
// middle resumes what its predecessor left suspended (Recover) instead of
// leaving it for nobody.
//
// The token is what makes the calls safe to retry and to arrive late. A Freeze
// repeated under the same token is the same hold. A Thaw under another token
// touches nothing. A Thaw that finds the hold already ended reports that the
// freeze did not last, which is the one answer its holder must not miss: what it
// took during the hold may not be consistent.
package freeze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMaxDeadline caps how long any hold may last on this host, whatever
	// its holder asks for.
	DefaultMaxDeadline = 30 * time.Second

	// defaultResumeTimeout bounds one resume. It runs on a context of its own,
	// because the call that ends a hold may be the one whose context expired.
	defaultResumeTimeout = 30 * time.Second

	// resumeAttempts is how often a failed resume is tried again before the
	// hold is reported as not resumed. The record stays in that case, so the
	// next start of the process tries once more.
	resumeAttempts = 3
)

var (
	// ErrHeld reports a Freeze of a device another holder has frozen.
	ErrHeld = errors.New("freeze: the volume is frozen under another token")

	// ErrTokenMismatch reports a Thaw under a token that is not the hold's.
	ErrTokenMismatch = errors.New("freeze: the volume is frozen under another token, and was left frozen")
)

// Mapper suspends and resumes device-mapper devices by name. devmapper.Mapper
// is the implementation.
type Mapper interface {
	Suspend(ctx context.Context, name string) error
	Resume(ctx context.Context, name string) error
}

// Timer is what AfterFunc returns: a pending call that Stop cancels, reporting
// whether it did so before the call started.
type Timer interface {
	Stop() bool
}

// Config is what Holds is built with.
type Config struct {
	// Mapper suspends and resumes. Required.
	Mapper Mapper

	// Dir is where a hold is recorded while it lasts. It has to outlive the
	// process, because the record is how a restarted process finds a device
	// its predecessor left suspended. Empty records nothing, which is for tests.
	Dir string

	// MaxDeadline caps every hold. Zero is DefaultMaxDeadline.
	MaxDeadline time.Duration

	// AfterFunc schedules the deadline. Nil is time.AfterFunc.
	AfterFunc func(time.Duration, func()) Timer

	// Now, when set, is read for the time instead of time.Now.
	Now func() time.Time

	// Logger records each hold's start, end, and length. Nil discards.
	Logger *slog.Logger
}

// Holds is every hold on this host, by key.
type Holds struct {
	cfg Config

	mu    sync.Mutex
	holds map[string]*hold
}

// hold is one frozen device.
type hold struct {
	token  string
	device string

	// pending is a hold whose suspend has not finished yet, and canceled one
	// whose holder thawed it before it did.
	pending  bool
	canceled bool

	since   time.Time
	timer   Timer
	release func()
}

// record is a hold on disk.
type record struct {
	Device  string    `json:"device"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// New returns the holds over cfg.
func New(cfg Config) *Holds {
	if cfg.MaxDeadline <= 0 {
		cfg.MaxDeadline = DefaultMaxDeadline
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Holds{cfg: cfg, holds: map[string]*hold{}}
}

// Acquire is what a Freeze takes before it suspends anything, and the release
// it answers is called once, when the hold ends however it ends. A node uses it
// for its per-volume lock, so nothing stages, unstages, or grows the volume
// while it is suspended. Nil acquires nothing.
type Acquire func(ctx context.Context) (release func(), err error)

// Freeze suspends device under key for at most deadline, capped by the host's
// maximum, and returns once it is suspended.
//
// A Freeze under the token of the hold already in place is that hold and
// changes nothing, which is what a retried call needs. Under another token it
// is ErrHeld, and the device is not suspended twice.
func (h *Holds) Freeze(
	ctx context.Context, key, device, token string, deadline time.Duration, acquire Acquire,
) error {
	if token == "" {
		return errors.New("freeze: a hold needs a token")
	}
	h.mu.Lock()
	if cur, ok := h.holds[key]; ok {
		h.mu.Unlock()
		if cur.token == token {
			return nil
		}
		return ErrHeld
	}
	hd := &hold{token: token, device: device, pending: true}
	h.holds[key] = hd
	h.mu.Unlock()

	fail := func(err error) error {
		h.mu.Lock()
		if h.holds[key] == hd {
			delete(h.holds, key)
		}
		h.mu.Unlock()
		return err
	}

	release := func() {}
	if acquire != nil {
		r, err := acquire(ctx)
		if err != nil {
			return fail(fmt.Errorf("freeze %s: %w", key, err))
		}
		release = r
	}

	if deadline <= 0 || deadline > h.cfg.MaxDeadline {
		deadline = h.cfg.MaxDeadline
	}
	if err := h.write(key, record{Device: device, Token: token, Expires: h.cfg.Now().Add(deadline)}); err != nil {
		release()
		return fail(err)
	}

	if err := h.cfg.Mapper.Suspend(ctx, device); err != nil {
		// A suspend that failed partway may still have taken effect, and
		// resuming a device that is not suspended is harmless, so it is resumed
		// rather than guessed about.
		hd.release = release
		_ = h.resume(key, hd)
		return fail(fmt.Errorf("freeze %s: %w", key, err))
	}

	h.mu.Lock()
	hd.release = release
	hd.since = h.cfg.Now()
	hd.pending = false
	if hd.canceled {
		// The holder thawed while the suspend ran. It was told the freeze did
		// not hold, so the device goes straight back.
		if h.holds[key] == hd {
			delete(h.holds, key)
		}
		h.mu.Unlock()
		if err := h.resume(key, hd); err != nil {
			return err
		}
		return fmt.Errorf("freeze %s: thawed while it was being frozen", key)
	}
	hd.timer = h.cfg.AfterFunc(deadline, func() { h.expire(key, hd) })
	h.mu.Unlock()

	h.cfg.Logger.Info("frozen", "volume", key, "device", device, "deadline", deadline)
	return nil
}

// Thaw ends the hold under key and reports whether it was still in place: true
// means the device was suspended without a break from Freeze until now, which
// is the guarantee its holder needs. False means it was not, because the
// deadline ended it first, because it was never frozen, or because this
// thawed it before its suspend finished, and what the holder took under it is
// not to be trusted.
//
// A Thaw under another token leaves the hold alone and is ErrTokenMismatch. An
// error with true is a hold that was in place and could not be resumed: what
// was taken under it is consistent, and the device needs attention.
func (h *Holds) Thaw(_ context.Context, key, token string) (bool, error) {
	h.mu.Lock()
	hd, ok := h.holds[key]
	switch {
	case !ok:
		h.mu.Unlock()
		return false, nil
	case hd.token != token:
		h.mu.Unlock()
		return false, ErrTokenMismatch
	case hd.pending:
		hd.canceled = true
		h.mu.Unlock()
		return false, nil
	case !hd.timer.Stop():
		// The deadline fired and its expiry is on its way. The hold ended
		// before this call, whoever gets to resume it.
		h.mu.Unlock()
		return false, nil
	}
	delete(h.holds, key)
	h.mu.Unlock()
	return true, h.resume(key, hd)
}

// expire ends a hold at its deadline.
func (h *Holds) expire(key string, hd *hold) {
	h.mu.Lock()
	if h.holds[key] != hd {
		h.mu.Unlock()
		return
	}
	delete(h.holds, key)
	h.mu.Unlock()
	h.cfg.Logger.Warn("freeze deadline reached, resuming", "volume", key, "device", hd.device)
	_ = h.resume(key, hd)
}

// resume resumes a hold's device, a few times if it has to, removes its record
// once it is resumed, and releases what the hold acquired either way.
func (h *Holds) resume(key string, hd *hold) error {
	defer func() {
		if hd.release != nil {
			hd.release()
		}
	}()
	var err error
	for range resumeAttempts {
		ctx, cancel := context.WithTimeout(context.Background(), defaultResumeTimeout)
		err = h.cfg.Mapper.Resume(ctx, hd.device)
		cancel()
		if err == nil {
			break
		}
	}
	if err != nil {
		h.cfg.Logger.Error("could not resume a frozen device; it is resumed on the next start",
			"volume", key, "device", hd.device, "error", err)
		return fmt.Errorf("freeze %s: %w", key, err)
	}
	if !hd.since.IsZero() {
		h.cfg.Logger.Info("thawed", "volume", key, "device", hd.device, "frozen", h.cfg.Now().Sub(hd.since))
	}
	return h.remove(key)
}

// Recover resumes every device a previous process recorded as frozen. The
// process that froze them is gone, and its deadlines with it, so each of them is
// resumed now, whether or not its deadline has passed. It is called once, at
// start, before any Freeze.
func (h *Holds) Recover(ctx context.Context) error {
	if h.cfg.Dir == "" {
		return nil
	}
	entries, err := os.ReadDir(h.cfg.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("freeze: read %s: %w", h.cfg.Dir, err)
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(h.cfg.Dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var rec record
		if err := json.Unmarshal(data, &rec); err != nil || rec.Device == "" {
			// Nothing in it names a device, so there is nothing to resume.
			_ = os.Remove(path)
			continue
		}
		if err := h.cfg.Mapper.Resume(ctx, rec.Device); err != nil {
			if strings.Contains(err.Error(), "No such device") {
				// The device is gone, which a reboot does, and with it the
				// suspension: there is nothing left to resume.
				_ = os.Remove(path)
				continue
			}
			errs = append(errs, fmt.Errorf("freeze: resume %s left by a previous process: %w", rec.Device, err))
			continue
		}
		h.cfg.Logger.Warn("resumed a device a previous process left frozen", "device", rec.Device)
		_ = os.Remove(path)
	}
	return errors.Join(errs...)
}

// path is a hold's record, named for its key made safe for a filename.
func (h *Holds) path(key string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '_'
		}
	}, key)
	return filepath.Join(h.cfg.Dir, safe+".json")
}

func (h *Holds) write(key string, rec record) error {
	if h.cfg.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(h.cfg.Dir, 0o700); err != nil {
		return fmt.Errorf("freeze: create %s: %w", h.cfg.Dir, err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := h.path(key) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("freeze: record %s: %w", key, err)
	}
	if err := os.Rename(tmp, h.path(key)); err != nil {
		return fmt.Errorf("freeze: record %s: %w", key, err)
	}
	return nil
}

func (h *Holds) remove(key string) error {
	if h.cfg.Dir == "" {
		return nil
	}
	if err := os.Remove(h.path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("freeze: remove the record of %s: %w", key, err)
	}
	return nil
}
