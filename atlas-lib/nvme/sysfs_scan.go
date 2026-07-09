package nvme

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/simplyblock/atlas/internal/sysfs"
)

// nsNameRE matches a block-namespace directory such as "nvme0n1". It
// deliberately excludes per-controller legs ("nvme0c0n1") and the generic
// char namespace ("ng0n1"); the host I/O device is the subsystem-level one.
var nsNameRE = regexp.MustCompile(`^nvme\d+n\d+$`)

// legNameRE matches a per-controller namespace leg such as "nvme0c1n1" —
// the ANA-bearing path under a controller directory.
var legNameRE = regexp.MustCompile(`^nvme\d+c\d+n\d+$`)

// scanSubsystems reads every NVMe subsystem under sysRoot, populating each
// with its namespaces and the controllers (paths) that front it.
func scanSubsystems(sysRoot, devRoot string) ([]Subsystem, error) {
	ctrls, err := scanControllers(sysRoot, devRoot)
	if err != nil {
		return nil, err
	}
	paths, err := scanPaths(sysRoot)
	if err != nil {
		return nil, err
	}

	names, err := sysfs.List(sysRoot, sysfs.ClassSubsystem)
	if err != nil {
		return nil, err
	}

	subs := make([]Subsystem, 0, len(names))
	for _, name := range names {
		base := filepath.Join(sysRoot, sysfs.ClassSubsystem, name)
		s := Subsystem{
			ID:          SubsystemID(name),
			SysfsPath:   base,
			NQN:         readStr(base, "subsysnqn"),
			Model:       readStr(base, "model"),
			Serial:      readStr(base, "serial"),
			FirmwareRev: readStr(base, "firmware_rev"),
			Type:        readStr(base, "subsystype"),
			IOPolicy:    readStr(base, "iopolicy"),
		}

		entries, err := sysfs.List(base)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if nsNameRE.MatchString(e) {
				s.Namespaces = append(s.Namespaces, scanNamespace(filepath.Join(base, e), devRoot, e))
			}
		}

		ctrlIDs := make(map[ControllerID]bool)
		for _, c := range ctrls {
			if c.NQN == s.NQN {
				s.Controllers = append(s.Controllers, c)
				ctrlIDs[c.ID] = true
			}
		}

		// Attach each ANA path to its namespace (matched by NSID), but only
		// paths whose owning controller belongs to this subsystem.
		for i := range s.Namespaces {
			for _, p := range paths {
				if ctrlIDs[p.Controller] && p.NSID == s.Namespaces[i].ID {
					s.Namespaces[i].Paths = append(s.Namespaces[i].Paths, p)
				}
			}
		}

		subs = append(subs, s)
	}
	return subs, nil
}

// scanDevices flattens the subsystems into attachable namespace devices.
func scanDevices(sysRoot, devRoot string) ([]Device, error) {
	subs, err := scanSubsystems(sysRoot, devRoot)
	if err != nil {
		return nil, err
	}
	var devs []Device
	for _, s := range subs {
		for _, ns := range s.Namespaces {
			devs = append(devs, Device{Namespace: ns, Subsystem: s})
		}
	}
	return devs, nil
}

func scanControllers(sysRoot, devRoot string) ([]Controller, error) {
	names, err := sysfs.List(sysRoot, sysfs.ClassNVMe)
	if err != nil {
		return nil, err
	}
	out := make([]Controller, 0, len(names))
	for _, name := range names {
		base := filepath.Join(sysRoot, sysfs.ClassNVMe, name)
		out = append(out, Controller{
			ID:                ControllerID(name),
			SysfsPath:         base,
			DevicePath:        filepath.Join(devRoot, name),
			Dev:               readStr(base, "dev"),
			NQN:               readStr(base, "subsysnqn"),
			CntlID:            readU16(base, "cntlid"),
			Type:              readStr(base, "cntrltype"),
			Transport:         readStr(base, "transport"),
			State:             readStr(base, "state"),
			Address:           parseAddress(readStr(base, "address")),
			HostNQN:           readStr(base, "hostnqn"),
			HostID:            readStr(base, "hostid"),
			NUMANode:          readInt(base, "numa_node", -1),
			QueueCount:        readInt(base, "queue_count", 0),
			SQSize:            readInt(base, "sqsize", 0),
			KeepAliveTOSec:    readInt(base, "kato", 0),
			CtrlLossTMOSec:    readInt(base, "ctrl_loss_tmo", 0),
			ReconnectDelaySec: readInt(base, "reconnect_delay", 0),
			FastIOFailTMO:     readStr(base, "fast_io_fail_tmo"),
		})
	}
	return out, nil
}

// scanPaths reads every per-controller namespace leg (nvmeXcYnZ) across all
// controllers, capturing its ANA state. Paths are later grouped onto their
// namespace by NSID.
func scanPaths(sysRoot string) ([]Path, error) {
	ctrlNames, err := sysfs.List(sysRoot, sysfs.ClassNVMe)
	if err != nil {
		return nil, err
	}
	var paths []Path
	for _, cn := range ctrlNames {
		cbase := filepath.Join(sysRoot, sysfs.ClassNVMe, cn)
		legs, err := sysfs.List(cbase)
		if err != nil {
			return nil, err
		}
		for _, leg := range legs {
			if !legNameRE.MatchString(leg) {
				continue
			}
			lbase := filepath.Join(cbase, leg)
			paths = append(paths, Path{
				Controller: ControllerID(cn),
				Name:       leg,
				SysfsPath:  lbase,
				NSID:       NamespaceID(readU32(lbase, "nsid")),
				ANAState:   ANAState(readStr(lbase, "ana_state")),
				ANAGroupID: readU32(lbase, "ana_grpid"),
			})
		}
	}
	return paths, nil
}

func scanNamespace(base, devRoot, name string) Namespace {
	return Namespace{
		ID:               NamespaceID(readU32(base, "nsid")),
		Name:             name,
		SysfsPath:        base,
		DevicePath:       filepath.Join(devRoot, name),
		Dev:              readStr(base, "dev"),
		UUID:             readStr(base, "uuid"),
		NGUID:            readStr(base, "nguid"),
		WWID:             readStr(base, "wwid"),
		CSI:              readInt(base, "csi", 0),
		LogicalBlockSize: readU32(base, "queue/logical_block_size"),
		Capacity:         readU64(base, "size"),
		Used:             readU64(base, "nuse"),
		MetadataBytes:    readU32(base, "metadata_bytes"),
		ReadOnly:         readBool(base, "ro"),
		Hidden:           readBool(base, "hidden"),
	}
}

// parseAddress parses an NVMe-oF "address" attribute such as
// "traddr=192.168.10.69,trsvcid=4426,src_addr=192.168.10.67".
func parseAddress(s string) Address {
	var a Address
	for kv := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "traddr":
			a.TrAddr = strings.TrimSpace(v)
		case "trsvcid":
			a.TrSvcID = strings.TrimSpace(v)
		case "src_addr":
			a.SrcAddr = strings.TrimSpace(v)
		}
	}
	return a
}

// --- attribute readers (missing/unparseable values fall back to zero) ---

func readStr(base, attr string) string {
	s, _ := sysfs.ReadAttr(base, attr)
	return s
}

func readInt(base, attr string, def int) int {
	s := readStr(base, attr)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func readU16(base, attr string) uint16 {
	n, _ := strconv.ParseUint(readStr(base, attr), 10, 16)
	return uint16(n)
}

func readU32(base, attr string) uint32 {
	n, _ := strconv.ParseUint(readStr(base, attr), 10, 32)
	return uint32(n)
}

func readU64(base, attr string) uint64 {
	n, _ := strconv.ParseUint(readStr(base, attr), 10, 64)
	return n
}

func readBool(base, attr string) bool {
	return readStr(base, attr) == "1"
}
