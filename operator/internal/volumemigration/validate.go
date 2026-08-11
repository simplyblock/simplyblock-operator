package volumemigration

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Connection describes one NVMe-oF target path to connect and validate.
type Connection struct {
	NQN            string `json:"nqn"`
	IP             string `json:"ip"`
	Port           int    `json:"port"`
	Transport      string `json:"transport"`
	NrIoQueues     int    `json:"nrIoQueues,omitempty"`
	ReconnectDelay int    `json:"reconnectDelay,omitempty"`
	CtrlLossTmo    int    `json:"ctrlLossTmo,omitempty"`
	FastIOFailTmo  int    `json:"fastIOFailTmo,omitempty"`
	KeepAliveTmo   int    `json:"keepAliveTmo,omitempty"`
}

// EnsureMigrationPaths connects each NVMe-oF path. An "already connected"
// response from nvme-cli is treated as success.
//
// The connect's own success is not proof that the path exists: nvme-cli can return
// zero while the kernel keeps retrying an admin-queue connect the target refuses (for
// instance because the subsystem does not exist there yet). VerifyMigrationPaths is
// what establishes that, by reading the host's own view afterwards.
func EnsureMigrationPaths(conns []Connection) error {
	for _, c := range conns {
		if err := nvmeConnect(c); err != nil {
			return err
		}
	}
	return nil
}

// connectArgs builds the `nvme` argument list for one path. Optional tuning flags
// are only passed when set, so a zero value means "leave the kernel default" rather
// than "pass 0" — nvme-cli rejects some zeros and silently accepts others.
func connectArgs(c Connection) []string {
	args := []string{
		"connect",
		"-t", c.Transport,
		"-a", c.IP,
		"-s", strconv.Itoa(c.Port),
		"-n", c.NQN,
	}
	if c.NrIoQueues > 0 {
		args = append(args, fmt.Sprintf("--nr-io-queues=%d", c.NrIoQueues))
	}
	if c.ReconnectDelay > 0 {
		args = append(args, fmt.Sprintf("--reconnect-delay=%d", c.ReconnectDelay))
	}
	if c.CtrlLossTmo > 0 {
		args = append(args, fmt.Sprintf("--ctrl-loss-tmo=%d", c.CtrlLossTmo))
	}
	if c.FastIOFailTmo > 0 {
		args = append(args, fmt.Sprintf("--fast_io_fail_tmo=%d", c.FastIOFailTmo))
	}
	if c.KeepAliveTmo > 0 {
		args = append(args, fmt.Sprintf("--keep-alive-tmo=%d", c.KeepAliveTmo))
	}
	return args
}

func nvmeConnect(c Connection) error {
	args := connectArgs(c)
	out, err := exec.Command("sudo", append([]string{"nvme"}, args...)...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already connected") {
		return fmt.Errorf("nvme connect %s@%s:%d: %w: %s", c.NQN, c.IP, c.Port, err, strings.TrimSpace(string(out)))
	}
	return nil
}
