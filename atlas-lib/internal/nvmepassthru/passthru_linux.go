//go:build linux

// Package nvmepassthru is the kernel's NVMe passthrough command, the one struct
// both package nvme (an Identify on a controller) and package nvmepr (a
// Reservation Report on a namespace) fill in. It lives here so the two share one
// copy of a layout the kernel copies in and out byte for byte.
package nvmepassthru

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The ioctl request numbers: _IOWR('N', nr, struct nvme_passthru_cmd) with the
// 72-byte struct, and NVME_IOCTL_ID, which is _IO('N', 0x40).
const (
	IoctlID       = 0x4E40
	IoctlAdminCmd = 0xC0484E41
	IoctlIOCmd    = 0xC0484E43
)

// Cmd mirrors the kernel's struct nvme_passthru_cmd from
// include/uapi/linux/nvme_ioctl.h (72 bytes). Field order and widths must match
// exactly, because the ioctl copies the struct in and out.
type Cmd struct {
	Opcode      uint8
	Flags       uint8
	Rsvd1       uint16
	NSID        uint32
	CDW2        uint32
	CDW3        uint32
	Metadata    uint64
	Addr        uint64
	MetadataLen uint32
	DataLen     uint32
	CDW10       uint32
	CDW11       uint32
	CDW12       uint32
	CDW13       uint32
	CDW14       uint32
	CDW15       uint32
	TimeoutMs   uint32
	Result      uint32
}

// Run issues cmd with the ioctl request on fd. data is the buffer cmd.Addr
// points at, kept alive until the kernel has finished writing into it.
func Run(fd uintptr, request uintptr, cmd *Cmd, data []byte) error {
	if len(data) > 0 {
		cmd.Addr = uint64(uintptr(unsafe.Pointer(&data[0])))
		cmd.DataLen = uint32(len(data))
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(cmd)))
	runtime.KeepAlive(data)
	if errno != 0 {
		return errno
	}
	return nil
}

// NamespaceID asks a namespace block device for its namespace identifier.
func NamespaceID(fd uintptr) (uint32, error) {
	nsid, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, IoctlID, 0)
	if errno != 0 {
		return 0, errno
	}
	return uint32(nsid), nil
}
