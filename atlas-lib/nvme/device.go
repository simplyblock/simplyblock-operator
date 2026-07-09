package nvme

// Identifiers for the three sysfs object kinds. Values are the kernel's
// own names, e.g. SubsystemID("nvme-subsys0"), ControllerID("nvme0").
type (
	SubsystemID  string
	ControllerID string
	// NamespaceID is the NVMe namespace identifier (NSID).
	NamespaceID uint32
)

// Subsystem is an NVMe subsystem as exposed under
// /sys/class/nvme-subsystem/nvme-subsysN. For NVMe-oF it is the unit a
// simplyblock logical volume maps to: one subsystem fronted by one or more
// controllers (multipath / HA across storage nodes) exporting one or more
// namespaces. The block device the host uses (e.g. /dev/nvme0n1) is the
// subsystem-level multipath head, not any single controller's path.
type Subsystem struct {
	ID        SubsystemID // "nvme-subsys0"
	SysfsPath string      // "/sys/class/nvme-subsystem/nvme-subsys0"

	NQN         string // subsysnqn
	Model       string // model (simplyblock encodes the lvol UUID here)
	Serial      string // serial
	FirmwareRev string // firmware_rev
	Type        string // subsystype, e.g. "nvm", "discovery"
	IOPolicy    string // iopolicy, e.g. "numa", "round-robin", "queue-depth"

	Controllers []Controller // the paths/legs fronting this subsystem
	Namespaces  []Namespace  // namespaces exported by the subsystem
}

// Address is a parsed NVMe-oF controller address (the "address" attribute),
// e.g. "traddr=192.168.10.69,trsvcid=4426,src_addr=192.168.10.67".
type Address struct {
	TrAddr  string // traddr  — target transport address
	TrSvcID string // trsvcid — target service id (port)
	SrcAddr string // src_addr — host source address (may be empty)
}

// Controller is one path into a subsystem, exposed under
// /sys/class/nvme/nvmeN. For NVMe-oF the transport/address fields describe
// the fabric link; a subsystem with multiple live controllers is multipath.
type Controller struct {
	ID         ControllerID // "nvme0"
	SysfsPath  string       // "/sys/class/nvme/nvme0"
	DevicePath string       // "/dev/nvme0" (char/admin device)
	Dev        string       // dev, "major:minor" e.g. "238:0"

	NQN       string  // subsysnqn
	CntlID    uint16  // cntlid — controller id within the subsystem
	Type      string  // cntrltype, e.g. "io", "discovery", "admin"
	Transport string  // transport, e.g. "tcp", "rdma", "fc", "pcie"
	State     string  // state, e.g. "live", "connecting", "resetting"
	Address   Address // parsed "address" attribute

	HostNQN  string // hostnqn
	HostID   string // hostid
	NUMANode int    // numa_node (-1 when unset)

	QueueCount int // queue_count
	SQSize     int // sqsize

	// Reconnect / timeout tunables (seconds; FastIOFailTMO may be "off").
	KeepAliveTOSec    int    // kato
	CtrlLossTMOSec    int    // ctrl_loss_tmo
	ReconnectDelaySec int    // reconnect_delay
	FastIOFailTMO     string // fast_io_fail_tmo, "off" or seconds
}

// ANAState is the Asymmetric Namespace Access state of a path to a
// namespace (NVMe multipath), as reported by the kernel ana_state attribute.
type ANAState string

const (
	ANAOptimized      ANAState = "optimized"
	ANANonOptimized   ANAState = "non-optimized"
	ANAInaccessible   ANAState = "inaccessible"
	ANAPersistentLoss ANAState = "persistent-loss"
	ANAChange         ANAState = "change"
)

// Accessible reports whether I/O may be issued over a path in this state
// (optimized or non-optimized). The other states carry no servable I/O.
func (s ANAState) Accessible() bool {
	return s == ANAOptimized || s == ANANonOptimized
}

// Path is one controller's access path to a namespace — the per-controller
// leg exposed under /sys/class/nvme/nvmeN/nvmeXcYnZ. With multiple
// controllers (multipath/HA) a namespace has one Path per controller, each
// with its own ANA state; the kernel routes I/O to optimized paths first.
type Path struct {
	Controller ControllerID // owning controller, e.g. "nvme1"
	Name       string       // leg name, e.g. "nvme0c1n1"
	SysfsPath  string       // ".../nvme/nvme1/nvme0c1n1"
	NSID       NamespaceID  // namespace id this path serves

	ANAState   ANAState // ana_state, e.g. "optimized", "non-optimized"
	ANAGroupID uint32   // ana_grpid
}

// Namespace is a block namespace exported by a subsystem, exposed under
// /sys/class/nvme-subsystem/nvme-subsysN/nvmeXnY — the multipath block
// head whose device node is DevicePath.
type Namespace struct {
	ID         NamespaceID // nsid
	Name       string      // "nvme0n1"
	SysfsPath  string      // ".../nvme-subsys0/nvme0n1"
	DevicePath string      // "/dev/nvme0n1"
	Dev        string      // dev, "major:minor" e.g. "259:1"

	UUID  string // uuid  — namespace UUID (simplyblock: the lvol UUID)
	NGUID string // nguid
	WWID  string // wwid, e.g. "uuid.<uuid>"
	CSI   int    // csi — command set identifier (0 = NVM)

	LogicalBlockSize uint32 // queue/logical_block_size, bytes
	// Capacity and Used are in 512-byte sectors (sysfs "size"/"nuse"),
	// independent of LogicalBlockSize. Multiply by 512 for bytes.
	Capacity      uint64 // size
	Used          uint64 // nuse
	MetadataBytes uint32 // metadata_bytes

	ReadOnly bool // ro
	Hidden   bool // hidden

	// Paths is the per-controller ANA view of this namespace (one entry
	// per controller fronting the subsystem). Empty for non-multipath
	// devices.
	Paths []Path
}

// Device is a resolved, attachable namespace together with the subsystem
// that exports it — and thus all of its controller paths. It is the unit a
// CSI NodeStage/NodePublish operation acts on.
type Device struct {
	Namespace Namespace
	Subsystem Subsystem
}
