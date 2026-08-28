package nvme

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/simplyblock/atlas/internal/sysfs"
)

// nsNameRE matches a block-namespace directory such as `nvme0n1`. It
// deliberately excludes per-controller legs (`nvme0c0n1`) and the generic
// char namespace (`ng0n1`); the host I/O device is the subsystem-level one.
var nsNameRE = regexp.MustCompile(`^nvme\d+n\d+$`)

// legNameRE matches a per-controller namespace leg such as "nvme0c1n1" —
// the ANA-bearing path under a controller directory.
var legNameRE = regexp.MustCompile(`^nvme\d+c\d+n\d+$`)

// ctrlNameRE matches a controller entry such as `nvme0`. The kernel links
// every controller into its subsystem's directory, which is how a controller
// is tied to the subsystem it fronts.
var ctrlNameRE = regexp.MustCompile(`^nvme\d+$`)

// legHeadRE splits a leg name into the head it serves and the controller it
// runs over: `nvme0c1n1` -> head `nvme0` + `n1`, i.e., head device `nvme0n1`
// reached via controller `nvme1`.
var legHeadRE = regexp.MustCompile(`^(nvme\d+)c\d+(n\d+)$`)

// legHead returns the name of the namespace head a leg serves, or "" if leg is
// not a leg name. Two subsystems can front the same NQN — a stale one the
// kernel has yet to reap next to a fresh one — and their namespaces then have
// the same NSID, so NSID alone cannot say which head a leg belongs to.
func legHead(leg string) string {
	m := legHeadRE.FindStringSubmatch(leg)
	if m == nil {
		return ""
	}
	return m[1] + m[2]
}

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

		s.Controllers = subsystemControllers(s, entries, ctrls)
		ctrlIDs := make(map[ControllerID]bool, len(s.Controllers))
		for _, c := range s.Controllers {
			ctrlIDs[c.ID] = true
		}

		// Attach each ANA path to the namespace head it serves. The head is
		// named by the leg itself, so two subsystems fronting one NQN keep
		// their own ANA view instead of inheriting each other's paths — which
		// is what lets a caller tell a live subsystem from a stale one.
		for i := range s.Namespaces {
			for _, p := range paths {
				if ctrlIDs[p.Controller] && legHead(p.Name) == s.Namespaces[i].Name && p.NSID == s.Namespaces[i].ID {
					s.Namespaces[i].Paths = append(s.Namespaces[i].Paths, p)
				}
			}
		}

		// Without a multipath head (nvme_core.multipath=0) the namespaces are
		// each controller's own block devices instead, so collect those too.
		s.Namespaces = append(s.Namespaces, controllerNamespaces(s, devRoot)...)

		subs = append(subs, s)
	}
	return subs, nil
}

// subsystemControllers returns the controllers fronting s, preferring the
// controller links the kernel places in the subsystem's own directory. That
// association is exact, where matching on the NQN is not: two subsystems can
// carry the same NQN while a stale one awaits reaping, and NQN matching would
// then hand each of them all of the other's controllers, making a dead
// subsystem indistinguishable from a live one. NQN matching stays as the
// fallback for trees that carry no controller links.
func subsystemControllers(s Subsystem, entries []string, ctrls []Controller) []Controller {
	byID := make(map[ControllerID]Controller, len(ctrls))
	for _, c := range ctrls {
		byID[c.ID] = c
	}

	var out []Controller
	for _, e := range entries {
		if !ctrlNameRE.MatchString(e) {
			continue
		}
		if c, ok := byID[ControllerID(e)]; ok {
			out = append(out, c)
		}
	}
	if len(out) > 0 {
		return out
	}

	for _, c := range ctrls {
		if c.NQN == s.NQN {
			out = append(out, c)
		}
	}
	return out
}

// controllerNamespaces returns the namespaces owned by s's controllers rather
// than by s itself — the layout when native NVMe multipath is off
// (nvme_core.multipath=0): the kernel builds no subsystem-level head, and each
// controller exposes its own block device for a namespace, so one volume
// reached over several paths becomes several devices sharing a namespace UUID
// (see Device.Siblings). Names already claimed by a head are skipped, so a
// kernel that links a head under its controller too is not counted twice.
func controllerNamespaces(s Subsystem, devRoot string) []Namespace {
	heads := make(map[string]bool, len(s.Namespaces))
	for _, ns := range s.Namespaces {
		heads[ns.Name] = true
	}

	var out []Namespace
	for _, c := range s.Controllers {
		entries, err := sysfs.List(c.SysfsPath)
		if err != nil {
			continue // a controller removed mid-scan is not this scan's problem
		}
		for _, e := range entries {
			if !nsNameRE.MatchString(e) || heads[e] {
				continue
			}
			ns := scanNamespace(filepath.Join(c.SysfsPath, e), devRoot, e)
			ns.Controller = c.ID
			out = append(out, ns)
			heads[e] = true
		}
	}
	return out
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

// parseAddress parses an NVMe-oF `address` attribute such as
// `traddr=192.168.10.69,trsvcid=4426,src_addr=192.168.10.67`.
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
