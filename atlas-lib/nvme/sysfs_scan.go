package nvme

import (
	"path/filepath"
	"regexp"
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
			NQN:         sysfs.String(base, "subsysnqn"),
			Model:       sysfs.String(base, "model"),
			Serial:      sysfs.String(base, "serial"),
			FirmwareRev: sysfs.String(base, "firmware_rev"),
			Type:        sysfs.String(base, "subsystype"),
			IOPolicy:    sysfs.String(base, "iopolicy"),
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
			Dev:               sysfs.String(base, "dev"),
			NQN:               sysfs.String(base, "subsysnqn"),
			CntlID:            sysfs.Uint16(base, "cntlid"),
			Type:              sysfs.String(base, "cntrltype"),
			Transport:         sysfs.String(base, "transport"),
			State:             sysfs.String(base, "state"),
			Address:           parseAddress(sysfs.String(base, "address")),
			HostNQN:           sysfs.String(base, "hostnqn"),
			HostID:            sysfs.String(base, "hostid"),
			NUMANode:          sysfs.Int(-1, base, "numa_node"),
			QueueCount:        sysfs.Int(0, base, "queue_count"),
			SQSize:            sysfs.Int(0, base, "sqsize"),
			KeepAliveTOSec:    sysfs.Int(0, base, "kato"),
			CtrlLossTMOSec:    sysfs.Int(0, base, "ctrl_loss_tmo"),
			ReconnectDelaySec: sysfs.Int(0, base, "reconnect_delay"),
			FastIOFailTMO:     sysfs.String(base, "fast_io_fail_tmo"),
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
				NSID:       NamespaceID(sysfs.Uint32(lbase, "nsid")),
				ANAState:   ANAState(sysfs.String(lbase, "ana_state")),
				ANAGroupID: sysfs.Uint32(lbase, "ana_grpid"),
			})
		}
	}
	return paths, nil
}

func scanNamespace(base, devRoot, name string) Namespace {
	return Namespace{
		ID:               NamespaceID(sysfs.Uint32(base, "nsid")),
		Name:             name,
		SysfsPath:        base,
		DevicePath:       filepath.Join(devRoot, name),
		Dev:              sysfs.String(base, "dev"),
		UUID:             sysfs.String(base, "uuid"),
		NGUID:            sysfs.String(base, "nguid"),
		WWID:             sysfs.String(base, "wwid"),
		CSI:              sysfs.Int(0, base, "csi"),
		LogicalBlockSize: sysfs.Uint32(base, "queue/logical_block_size"),
		Capacity:         sysfs.Uint64(base, "size"),
		Used:             sysfs.Uint64(base, "nuse"),
		MetadataBytes:    sysfs.Uint32(base, "metadata_bytes"),
		ReadOnly:         sysfs.Bool(base, "ro"),
		Hidden:           sysfs.Bool(base, "hidden"),
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
