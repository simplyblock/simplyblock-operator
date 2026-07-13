package volumemigration

import (
	"encoding/json"
	"fmt"
	"log"
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
func EnsureMigrationPaths(conns []Connection) error {
	for _, c := range conns {
		if err := nvmeConnect(c); err != nil {
			return err
		}
	}
	return nil
}

// ValidateMigrationPaths validates that an inaccessible ANA path exists for every
// expected (nqn, ip, port).
func ValidateMigrationPaths(conns []Connection) error {
	return validatePaths(conns)
}

func nvmeConnect(c Connection) error {
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
	out, err := exec.Command("sudo", append([]string{"nvme"}, args...)...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already connected") {
		return fmt.Errorf("nvme connect %s@%s:%d: %w: %s", c.NQN, c.IP, c.Port, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// nvme list -v -ojson output structures.
type nvmeListOutput struct {
	Devices []nvmeDevice `json:"Devices"`
}

type nvmeDevice struct {
	Subsystems []nvmeSubsystem `json:"Subsystems"`
}

type nvmeSubsystem struct {
	SubsystemNQN string           `json:"SubsystemNQN"`
	Controllers  []nvmeController `json:"Controllers"`
}

type nvmeController struct {
	Address string     `json:"Address"`
	Paths   []nvmePath `json:"Paths"`
}

type nvmePath struct {
	ANAState string `json:"ANAState"`
}

func validatePaths(conns []Connection) error {
	out, err := exec.Command("sudo", "nvme", "list", "-v", "-ojson").Output()
	if err != nil {
		return fmt.Errorf("nvme list: %w", err)
	}
	var list nvmeListOutput
	if err := json.Unmarshal(out, &list); err != nil {
		return fmt.Errorf("parse nvme list output: %w", err)
	}

	type connKey struct {
		NQN  string
		IP   string
		Port int
	}

	expected := make(map[connKey]struct{}, len(conns))
	for _, c := range conns {
		expected[connKey{c.NQN, c.IP, c.Port}] = struct{}{}
	}

	found := make(map[connKey]struct{})
	for _, dev := range list.Devices {
		for _, sub := range dev.Subsystems {
			for _, ctrl := range sub.Controllers {
				ip, port := parseAddress(ctrl.Address)
				k := connKey{sub.SubsystemNQN, ip, port}
				if _, ok := expected[k]; !ok {
					continue
				}
				for _, p := range ctrl.Paths {
					log.Printf("connection nqn=%s addr=%s:%d ana_state=%s", k.NQN, ip, port, p.ANAState)
					if p.ANAState == "inaccessible" {
						found[k] = struct{}{}
					}
				}
			}
		}
	}

	var missing []string
	for k := range expected {
		if _, ok := found[k]; !ok {
			missing = append(missing, fmt.Sprintf("%s@%s:%d", k.NQN, k.IP, k.Port))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("no inaccessible path found for: %s", strings.Join(missing, "; "))
	}
	return nil
}

func parseAddress(addr string) (ip string, port int) {
	for _, part := range strings.Split(addr, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "traddr":
			ip = kv[1]
		case "trsvcid":
			port, _ = strconv.Atoi(kv[1])
		}
	}
	return
}
