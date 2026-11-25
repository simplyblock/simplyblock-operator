// What a worker is known as to a storage array: its iSCSI initiator name, the
// port names of its Fibre Channel HBAs, and its NVMe host NQN.
//
// A multi-attach cluster's LUNs are presented to the workers by the array,
// which knows each host only by these. Reading them during discovery is what
// lets a deployment be checked before approval: a worker with no initiator for
// the array's protocol is one the array can never present a LUN to, and adding
// its node would fail after the cluster was already created.
//
// The files are the host's, so they are read under Config.HostRoot, for the
// reason the os-release reading is: a container carries its own /etc.

package inventory

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SANInitiators are a host's storage-network identities. A field is empty when
// the host has no such initiator configured.
type SANInitiators struct {
	// ISCSIIQN is the iSCSI initiator name (/etc/iscsi/initiatorname.iscsi).
	ISCSIIQN string
	// FCWWPNs are the port names of the Fibre Channel HBA ports, sixteen
	// lowercase hex digits each, without a 0x.
	FCWWPNs []string
	// NVMeHostNQN is the NVMe host NQN (/etc/nvme/hostnqn), which identifies
	// the host on NVMe/TCP, NVMe/RDMA, and NVMe/FC alike.
	NVMeHostNQN string
}

var wwpnPattern = regexp.MustCompile(`^0x[0-9a-f]{16}$`)

// ReadSANInitiators reads the host's initiators. A host without SAN software
// is not an error; an initiator file that exists and cannot be read is.
func ReadSANInitiators(cfg Config) (SANInitiators, error) {
	var out SANInitiators
	var errs []error

	iqn, err := readInitiatorName(filepath.Join(cfg.host(), "etc", "iscsi", "initiatorname.iscsi"))
	if err != nil {
		errs = append(errs, err)
	}
	out.ISCSIIQN = iqn

	nqn, err := readFirstLine(filepath.Join(cfg.host(), "etc", "nvme", "hostnqn"))
	if err != nil {
		errs = append(errs, err)
	}
	out.NVMeHostNQN = nqn

	hosts, err := os.ReadDir(cfg.sysfsPath("class", "fc_host"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("list the FC HBA ports: %w", err))
	}
	for _, host := range hosts {
		name, err := readFirstLine(cfg.sysfsPath("class", "fc_host", host.Name(), "port_name"))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		name = strings.ToLower(name)
		// An unconfigured port reports all zeroes.
		if wwpnPattern.MatchString(name) && strings.Trim(name[2:], "0") != "" {
			out.FCWWPNs = append(out.FCWWPNs, name[2:])
		}
	}
	sort.Strings(out.FCWWPNs)
	return out, errors.Join(errs...)
}

// readInitiatorName is the value of the first uncommented InitiatorName line.
func readInitiatorName(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if value, ok := strings.CutPrefix(line, "InitiatorName="); ok {
			return strings.TrimSpace(value), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return "", nil
}

// readFirstLine is the first line of a file, trimmed; empty for a missing one.
func readFirstLine(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(line), nil
}
