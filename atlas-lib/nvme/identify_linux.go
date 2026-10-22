package nvme

import (
	"fmt"
	"os"
	"time"

	"github.com/simplyblock/atlas/bounded"
	"github.com/simplyblock/atlas/internal/nvmepassthru"
)

const (
	nvmeAdminIdentify   = 0x06 // Identify admin command opcode
	nvmeIdentifyCNSCtrl = 0x01 // CNS value: Identify Controller data structure
)

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
	cmd := nvmepassthru.Cmd{
		Opcode:    nvmeAdminIdentify,
		NSID:      0,                   // Identify Controller ignores NSID
		CDW10:     nvmeIdentifyCNSCtrl, // CNS in the low byte
		TimeoutMs: identifyTimeoutMs,
	}
	if err := nvmepassthru.Run(f.Fd(), nvmepassthru.IoctlAdminCmd, &cmd, buf); err != nil {
		return 0, fmt.Errorf("ioctl NVME_ADMIN_CMD %s: %w", devicePath, err)
	}
	return mnanFromIdentify(buf), nil
}
