//go:build linux

package nvmepr

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/simplyblock/atlas/bounded"
	"github.com/simplyblock/atlas/internal/nvmepassthru"
)

// The request numbers of include/uapi/linux/pr.h: _IOW('p', nr, struct), that
// is (1<<30)|(sizeof<<16)|('p'<<8)|nr.
const (
	iocPRRegister     = 0x40000000 | 24<<16 | 'p'<<8 | 200
	iocPRReserve      = 0x40000000 | 16<<16 | 'p'<<8 | 201
	iocPRPreempt      = 0x40000000 | 24<<16 | 'p'<<8 | 203
	iocPRPreemptAbort = 0x40000000 | 24<<16 | 'p'<<8 | 204
)

// prFlagIgnoreKey is PR_FL_IGNORE_KEY.
const prFlagIgnoreKey = 1 << 0

type prRegistration struct {
	oldKey uint64
	newKey uint64
	flags  uint32
	pad    uint32
}

type prReservation struct {
	key   uint64
	typ   uint32
	flags uint32
}

// prPreempt names its keys from the NVMe command's side: oldKey is the key of
// the host that issues the command (CRKEY), and newKey is the key it removes
// (PRKEY).
type prPreempt struct {
	oldKey uint64
	newKey uint64
	typ    uint32
	flags  uint32
}

const (
	nvmeOpReservationReport = 0x0e
	reportEDS               = 1 // CDW11 bit 0: the extended data structure

	// commandTimeout bounds one command. A Preempt and Abort at the target
	// waits for the preempted host's commands, which SPDK abandons after 10
	// seconds, so the budget is longer than that.
	commandTimeout = 15 * time.Second

	// initialRegistrants sizes the first Report buffer. A namespace with more
	// registrants than that is read again with a buffer that fits them.
	initialRegistrants = 64
)

type linuxDevice struct {
	path string
	f    *os.File
}

// Open opens a namespace's block device (such as `/dev/nvme0n1`), which
// takes the reservation ioctls and the passthrough Report.
func Open(path string) (Device, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &linuxDevice{path: path, f: f}, nil
}

func (d *linuxDevice) Close() error { return d.f.Close() }

// ioctl runs one reservation ioctl under a deadline: a command no path can
// carry is held by the kernel until a controller reconnects or is deleted, and
// nothing in Go interrupts it.
func (d *linuxDevice) ioctl(op string, request uintptr, arg unsafe.Pointer) error {
	type result struct {
		status int
		errno  syscall.Errno
	}
	res, err := bounded.Call("pr "+op+" "+d.path, commandTimeout, func() (result, error) {
		r1, _, errno := unix.Syscall(unix.SYS_IOCTL, d.f.Fd(), request, uintptr(arg))
		return result{status: int(r1), errno: errno}, nil
	})
	if err != nil {
		return fmt.Errorf("%s %s: %w", op, d.path, err)
	}
	return statusError(op+" "+d.path, res.status, res.errno)
}

func (d *linuxDevice) Register(_ context.Context, oldKey, newKey uint64, ignoreExistingKey bool) error {
	reg := prRegistration{oldKey: oldKey, newKey: newKey}
	if ignoreExistingKey {
		reg.flags = prFlagIgnoreKey
	}
	return d.ioctl("register", iocPRRegister, unsafe.Pointer(&reg))
}

func (d *linuxDevice) Acquire(_ context.Context, key uint64, t Type) error {
	rsv := prReservation{key: key, typ: uint32(t)}
	return d.ioctl("acquire", iocPRReserve, unsafe.Pointer(&rsv))
}

func (d *linuxDevice) Preempt(_ context.Context, key, victim uint64, t Type, abort bool) error {
	p := prPreempt{oldKey: key, newKey: victim, typ: uint32(t)}
	request := uintptr(iocPRPreempt)
	if abort {
		request = iocPRPreemptAbort
	}
	return d.ioctl("preempt", request, unsafe.Pointer(&p))
}

func (d *linuxDevice) Report(_ context.Context) (Report, error) {
	nsid, err := nvmepassthru.NamespaceID(d.f.Fd())
	if err != nil {
		return Report{}, fmt.Errorf("namespace id of %s: %w", d.path, err)
	}
	for registrants := initialRegistrants; ; {
		buf, err := d.readReport(nsid, headerSize+registrants*entrySize)
		if err != nil {
			return Report{}, err
		}
		rep, err := ParseReport(buf)
		if err != nil {
			return Report{}, fmt.Errorf("report of %s: %w", d.path, err)
		}
		if need := reportedRegistrants(buf); need > registrants {
			registrants = need
			continue
		}
		return rep, nil
	}
}

// readReport sends one Reservation Report, rounding size up to whole dwords.
func (d *linuxDevice) readReport(nsid uint32, size int) ([]byte, error) {
	size = (size + 3) &^ 3
	return bounded.Call("pr report "+d.path, commandTimeout, func() ([]byte, error) {
		buf := make([]byte, size)
		cmd := nvmepassthru.Cmd{
			Opcode:    nvmeOpReservationReport,
			NSID:      nsid,
			CDW10:     uint32(size/4 - 1), // NUMD, zero-based dwords
			CDW11:     reportEDS,
			TimeoutMs: uint32(commandTimeout / time.Millisecond),
		}
		if err := nvmepassthru.Run(d.f.Fd(), nvmepassthru.IoctlIOCmd, &cmd, buf); err != nil {
			return nil, fmt.Errorf("reservation report of %s: %w", d.path, err)
		}
		return buf, nil
	})
}
