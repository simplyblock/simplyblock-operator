package nvme

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// nvmeIoctlAdminCmd is NVME_IOCTL_ADMIN_CMD: _IOWR('N', 0x41, struct
// nvme_passthru_cmd) with a 72-byte command struct — i.e.
// (3<<30)|(72<<16)|('N'<<8)|0x41.
const nvmeIoctlAdminCmd = 0xC0484E41

const (
	nvmeAdminIdentify    = 0x06 // Identify admin command opcode
	nvmeIdentifyCNSCtrl  = 0x01 // CNS value: Identify Controller data structure
	nvmeIdentifyDataLen  = 4096 // size of an Identify data structure
	nvmeIdentifyNNOffset = 516  // byte offset of NN (uint32, little-endian)
)

// nvmePassthruCmd mirrors the kernel's struct nvme_passthru_cmd from
// include/uapi/linux/nvme_ioctl.h (72 bytes). Field order and widths must
// match exactly; the ioctl copies this struct in and out.
type nvmePassthruCmd struct {
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

// identifyControllerNN issues an NVMe Identify Controller admin command on the
// controller character device (e.g. "/dev/nvme0") and returns its NN field —
// the maximum number of namespaces the controller's subsystem supports.
func identifyControllerNN(devicePath string) (uint32, error) {
	f, err := os.OpenFile(devicePath, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", devicePath, err)
	}
	defer f.Close()

	buf := make([]byte, nvmeIdentifyDataLen)
	cmd := nvmePassthruCmd{
		opcode:  nvmeAdminIdentify,
		nsid:    0, // Identify Controller ignores NSID
		addr:    uint64(uintptr(unsafe.Pointer(&buf[0]))),
		dataLen: nvmeIdentifyDataLen,
		cdw10:   nvmeIdentifyCNSCtrl, // CNS in the low byte
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), nvmeIoctlAdminCmd, uintptr(unsafe.Pointer(&cmd)))
	// Keep buf alive until the kernel has finished writing into it via addr.
	runtime.KeepAlive(buf)
	if errno != 0 {
		return 0, fmt.Errorf("ioctl NVME_ADMIN_CMD %s: %w", devicePath, errno)
	}
	return binary.LittleEndian.Uint32(buf[nvmeIdentifyNNOffset : nvmeIdentifyNNOffset+4]), nil
}
