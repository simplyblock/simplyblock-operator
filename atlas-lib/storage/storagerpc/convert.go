package storagerpc

import (
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/storage/storagerpc/storagev1"
)

// Conversion between the nvme snapshot types and their wire form.
//
// The mapping is total in both directions: every exported field of every type
// has a wire field, and nothing is derived, summarised or dropped. That is what
// lets a caller on the far side of a link ask the same questions of a device as
// a caller on the node — Accessible, Siblings, CoTenants and the rest are pure
// functions of these fields, so they answer identically once the snapshot
// crosses.
//
// Keeping it total is a maintenance obligation, not a property that holds by
// itself: a field added to nvme.Namespace and not to Namespace here compiles
// perfectly and silently reads as its zero value on the operator. The
// round-trip test is what catches that, and it is why it enumerates fully
// populated fixtures rather than a couple of representative ones.

func subsystemToProto(s nvme.Subsystem) *storagev1.Subsystem {
	out := &storagev1.Subsystem{
		Id:          string(s.ID),
		SysfsPath:   s.SysfsPath,
		Nqn:         s.NQN,
		Model:       s.Model,
		Serial:      s.Serial,
		FirmwareRev: s.FirmwareRev,
		Type:        s.Type,
		IoPolicy:    s.IOPolicy,
	}
	if len(s.Controllers) > 0 {
		out.Controllers = make([]*storagev1.Controller, len(s.Controllers))
		for i, c := range s.Controllers {
			out.Controllers[i] = controllerToProto(c)
		}
	}
	if len(s.Namespaces) > 0 {
		out.Namespaces = make([]*storagev1.Namespace, len(s.Namespaces))
		for i, ns := range s.Namespaces {
			out.Namespaces[i] = namespaceToProto(ns)
		}
	}
	return out
}

func subsystemFromProto(s *storagev1.Subsystem) nvme.Subsystem {
	if s == nil {
		return nvme.Subsystem{}
	}
	out := nvme.Subsystem{
		ID:          nvme.SubsystemID(s.GetId()),
		SysfsPath:   s.GetSysfsPath(),
		NQN:         s.GetNqn(),
		Model:       s.GetModel(),
		Serial:      s.GetSerial(),
		FirmwareRev: s.GetFirmwareRev(),
		Type:        s.GetType(),
		IOPolicy:    s.GetIoPolicy(),
	}
	if controllers := s.GetControllers(); len(controllers) > 0 {
		out.Controllers = make([]nvme.Controller, len(controllers))
		for i, c := range controllers {
			out.Controllers[i] = controllerFromProto(c)
		}
	}
	if namespaces := s.GetNamespaces(); len(namespaces) > 0 {
		out.Namespaces = make([]nvme.Namespace, len(namespaces))
		for i, ns := range namespaces {
			out.Namespaces[i] = namespaceFromProto(ns)
		}
	}
	return out
}

func controllerToProto(c nvme.Controller) *storagev1.Controller {
	return &storagev1.Controller{
		Id:         string(c.ID),
		SysfsPath:  c.SysfsPath,
		DevicePath: c.DevicePath,
		Dev:        c.Dev,

		Nqn:       c.NQN,
		Cntlid:    uint32(c.CntlID),
		Type:      c.Type,
		Transport: c.Transport,
		State:     c.State,
		Address: &storagev1.Address{
			Traddr:  c.Address.TrAddr,
			Trsvcid: c.Address.TrSvcID,
			SrcAddr: c.Address.SrcAddr,
		},

		HostNqn:  c.HostNQN,
		HostId:   c.HostID,
		NumaNode: int32(c.NUMANode),

		QueueCount: int32(c.QueueCount),
		SqSize:     int32(c.SQSize),

		KeepAliveToSec:    int32(c.KeepAliveTOSec),
		CtrlLossTmoSec:    int32(c.CtrlLossTMOSec),
		ReconnectDelaySec: int32(c.ReconnectDelaySec),
		FastIoFailTmo:     c.FastIOFailTMO,
	}
}

func controllerFromProto(c *storagev1.Controller) nvme.Controller {
	if c == nil {
		return nvme.Controller{}
	}
	return nvme.Controller{
		ID:         nvme.ControllerID(c.GetId()),
		SysfsPath:  c.GetSysfsPath(),
		DevicePath: c.GetDevicePath(),
		Dev:        c.GetDev(),

		NQN:       c.GetNqn(),
		CntlID:    uint16(c.GetCntlid()),
		Type:      c.GetType(),
		Transport: c.GetTransport(),
		State:     c.GetState(),
		Address: nvme.Address{
			TrAddr:  c.GetAddress().GetTraddr(),
			TrSvcID: c.GetAddress().GetTrsvcid(),
			SrcAddr: c.GetAddress().GetSrcAddr(),
		},

		HostNQN:  c.GetHostNqn(),
		HostID:   c.GetHostId(),
		NUMANode: int(c.GetNumaNode()),

		QueueCount: int(c.GetQueueCount()),
		SQSize:     int(c.GetSqSize()),

		KeepAliveTOSec:    int(c.GetKeepAliveToSec()),
		CtrlLossTMOSec:    int(c.GetCtrlLossTmoSec()),
		ReconnectDelaySec: int(c.GetReconnectDelaySec()),
		FastIOFailTMO:     c.GetFastIoFailTmo(),
	}
}

func namespaceToProto(ns nvme.Namespace) *storagev1.Namespace {
	out := &storagev1.Namespace{
		Id:         uint32(ns.ID),
		Name:       ns.Name,
		SysfsPath:  ns.SysfsPath,
		DevicePath: ns.DevicePath,
		Dev:        ns.Dev,

		Uuid:  ns.UUID,
		Nguid: ns.NGUID,
		Wwid:  ns.WWID,
		Csi:   int32(ns.CSI),

		LogicalBlockSize: ns.LogicalBlockSize,
		Capacity:         ns.Capacity,
		Used:             ns.Used,
		MetadataBytes:    ns.MetadataBytes,

		ReadOnly: ns.ReadOnly,
		Hidden:   ns.Hidden,

		Controller: string(ns.Controller),
	}
	if len(ns.Paths) > 0 {
		out.Paths = make([]*storagev1.Path, len(ns.Paths))
		for i, p := range ns.Paths {
			out.Paths[i] = pathToProto(p)
		}
	}
	return out
}

func namespaceFromProto(ns *storagev1.Namespace) nvme.Namespace {
	if ns == nil {
		return nvme.Namespace{}
	}
	out := nvme.Namespace{
		ID:         nvme.NamespaceID(ns.GetId()),
		Name:       ns.GetName(),
		SysfsPath:  ns.GetSysfsPath(),
		DevicePath: ns.GetDevicePath(),
		Dev:        ns.GetDev(),

		UUID:  ns.GetUuid(),
		NGUID: ns.GetNguid(),
		WWID:  ns.GetWwid(),
		CSI:   int(ns.GetCsi()),

		LogicalBlockSize: ns.GetLogicalBlockSize(),
		Capacity:         ns.GetCapacity(),
		Used:             ns.GetUsed(),
		MetadataBytes:    ns.GetMetadataBytes(),

		ReadOnly: ns.GetReadOnly(),
		Hidden:   ns.GetHidden(),

		Controller: nvme.ControllerID(ns.GetController()),
	}
	if paths := ns.GetPaths(); len(paths) > 0 {
		out.Paths = make([]nvme.Path, len(paths))
		for i, p := range paths {
			out.Paths[i] = pathFromProto(p)
		}
	}
	return out
}

func pathToProto(p nvme.Path) *storagev1.Path {
	return &storagev1.Path{
		Controller: string(p.Controller),
		Name:       p.Name,
		SysfsPath:  p.SysfsPath,
		Nsid:       uint32(p.NSID),
		AnaState:   string(p.ANAState),
		AnaGroupId: p.ANAGroupID,
	}
}

func pathFromProto(p *storagev1.Path) nvme.Path {
	if p == nil {
		return nvme.Path{}
	}
	return nvme.Path{
		Controller: nvme.ControllerID(p.GetController()),
		Name:       p.GetName(),
		SysfsPath:  p.GetSysfsPath(),
		NSID:       nvme.NamespaceID(p.GetNsid()),
		ANAState:   nvme.ANAState(p.GetAnaState()),
		ANAGroupID: p.GetAnaGroupId(),
	}
}

func deviceToProto(d nvme.Device) *storagev1.Device {
	return &storagev1.Device{
		Namespace: namespaceToProto(d.Namespace),
		Subsystem: subsystemToProto(d.Subsystem),
	}
}

// deviceFromProto rebuilds a device. The result carries no resolver — binding
// it is the caller's job, and [DeviceResolver] does it for every device it
// hands out, so that Siblings and CoTenants re-scan the node the device came
// from rather than failing as unbound.
func deviceFromProto(d *storagev1.Device) nvme.Device {
	if d == nil {
		return nvme.Device{}
	}
	return nvme.Device{
		Namespace: namespaceFromProto(d.GetNamespace()),
		Subsystem: subsystemFromProto(d.GetSubsystem()),
	}
}

func selectorToProto(sel nvme.DeviceSelector) *storagev1.DeviceSelector {
	return &storagev1.DeviceSelector{
		Nqn:        sel.NQN,
		Nsid:       uint32(sel.NSID),
		Uuid:       sel.UUID,
		DevicePath: sel.DevicePath,
	}
}

func selectorFromProto(sel *storagev1.DeviceSelector) nvme.DeviceSelector {
	if sel == nil {
		return nvme.DeviceSelector{}
	}
	return nvme.DeviceSelector{
		NQN:        sel.GetNqn(),
		NSID:       nvme.NamespaceID(sel.GetNsid()),
		UUID:       sel.GetUuid(),
		DevicePath: sel.GetDevicePath(),
	}
}
