//go:build linux

// The two NVMe passthrough commands the reservation code needs and the kernel's
// pr.h ioctls do not cover: the namespace ID and the Reservation Report. Only
// these two are built here, so no caller can send any other command.

package nvmepr

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The ioctl request numbers: NVME_IOCTL_ID is _IO('N', 0x40), and
// NVME_IOCTL_IO_CMD is _IOWR('N', 0x43, struct nvme_passthru_cmd).
const (
	ioctlNamespaceID = 0x4E40
	ioctlIOCmd       = 0xC0484E43
)

// passthruCmd mirrors the kernel's struct nvme_passthru_cmd from
// include/uapi/linux/nvme_ioctl.h (72 bytes). Field order and widths must match
// exactly, because the ioctl copies the struct in and out.
type passthruCmd struct {
	opcode      uint8
	flags       uint8
	rsvd1       uint16
	nsid        uint32
	cdw2        uint32
	cdw3        uint32
	metadata    uint64
	addr        uint64
	metadataLen uint32
	dataLen     uint32
	cdw10       uint32
	cdw11       uint32
	cdw12       uint32
	cdw13       uint32
	cdw14       uint32
	cdw15       uint32
	timeoutMs   uint32
	result      uint32
}

// rawSyscall is unix.Syscall, replaced in tests.
var rawSyscall = unix.Syscall

// namespaceID asks a namespace block device for its namespace identifier.
func namespaceID(fd uintptr) (uint32, error) {
	nsid, _, errno := rawSyscall(unix.SYS_IOCTL, fd, ioctlNamespaceID, 0)
	if errno != 0 {
		return 0, errno
	}
	return uint32(nsid), nil
}

// reservationReport sends a Reservation Report with the extended data structure
// for nsid into buf, whose length must be a whole number of dwords.
func reservationReport(fd uintptr, nsid uint32, buf []byte, timeoutMs uint32) error {
	cmd := passthruCmd{
		opcode:    nvmeOpReservationReport,
		nsid:      nsid,
		cdw10:     uint32(len(buf)/4 - 1), // NUMD, zero-based dwords
		cdw11:     reportEDS,
		timeoutMs: timeoutMs,
		addr:      uint64(uintptr(unsafe.Pointer(&buf[0]))),
		dataLen:   uint32(len(buf)),
	}
	status, _, errno := rawSyscall(unix.SYS_IOCTL, fd, ioctlIOCmd, uintptr(unsafe.Pointer(&cmd)))
	runtime.KeepAlive(buf)
	if errno != 0 {
		return errno
	}
	// A command the controller rejected returns its NVMe status with errno 0.
	if status != 0 {
		return fmt.Errorf("the controller rejected the command: status code type %d, status code %#x",
			(status>>8)&0x7, status&0xff)
	}
	return nil
}
