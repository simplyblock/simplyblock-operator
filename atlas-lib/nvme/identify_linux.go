package nvme

import (
	"fmt"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/simplyblock/atlas/bounded"
)

// nvmeIoctlAdminCmd is NVME_IOCTL_ADMIN_CMD: _IOWR('N', 0x41, struct
// nvme_passthru_cmd) with a 72-byte command struct, i.e.,
// (3<<30)|(72<<16)|('N'<<8)|0x41.
const nvmeIoctlAdminCmd = 0xC0484E41

const (
	nvmeAdminIdentify   = 0x06 // Identify admin command opcode
	nvmeIdentifyCNSCtrl = 0x01 // CNS value: Identify Controller data structure
)

// nvmePassthruCmd mirrors the kernel's struct nvme_passthru_cmd from
// include/uapi/linux/nvme_ioctl.h (72 bytes). Field order and widths must
// match exactly, because the ioctl copies this struct in and out
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

// identifyTimeoutMs is the command timeout handed to the kernel with the
// Identify, so a command that reaches the controller is failed by the kernel
// instead of waiting out the default admin timeout.
const identifyTimeoutMs = uint32(bounded.ReadTimeout / time.Millisecond)

// identifyControllerMNAN issues an NVMe Identify Controller admin command on
// the controller character device (e.g., "/dev/nvme0") and returns its MNAN
// field (Maximum Number of Allowed Namespaces), the most namespaces the
// controller's subsystem may hold.
//
// The open and the ioctl run under bounded.ReadTimeout. A controller can leave
// the live state between the sysfs read that chose it and the ioctl, and the
// kernel then holds the command until the controller reconnects or is deleted.
func identifyControllerMNAN(devicePath string) (uint32, error) {
	return bounded.Call("identify "+devicePath, bounded.ReadTimeout, func() (uint32, error) {
		return identifyControllerMNANUnbounded(devicePath)
	})
}

func identifyControllerMNANUnbounded(devicePath string) (uint32, error) {
	f, err := os.OpenFile(devicePath, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", devicePath, err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, identifyControllerLen)
	cmd := nvmePassthruCmd{
		opcode:    nvmeAdminIdentify,
		nsid:      0, // Identify Controller ignores NSID
		addr:      uint64(uintptr(unsafe.Pointer(&buf[0]))),
		dataLen:   identifyControllerLen,
		cdw10:     nvmeIdentifyCNSCtrl, // CNS in the low byte
		timeoutMs: identifyTimeoutMs,
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), nvmeIoctlAdminCmd, uintptr(unsafe.Pointer(&cmd)))
	// Keep buf alive until the kernel has finished writing into it via addr.
	runtime.KeepAlive(buf)
	if errno != 0 {
		return 0, fmt.Errorf("ioctl NVME_ADMIN_CMD %s: %w", devicePath, errno)
	}
	return mnanFromIdentify(buf), nil
}
