// Building a ClusterDeploymentConfig from choices made per worker: which node
// set it goes into, which devices and interfaces it hands over, and its failure
// domain, plus the cluster settings once for the document.
//
// The choices are per worker because that is how a support agent thinks about a
// fleet, and the document is per group because that is how the API states it:
// one group is the workers that share their devices, interfaces, and failure
// domain. Build does the grouping, so the agent never has to. A document the
// bundle already carries is the starting point for one of the same name, so the
// fields this editor does not show (tolerations, images, KMS, labels) survive an
// edit.

package cdcedit

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/discovery"
	"github.com/simplyblock/simplyblock-operator/internal/erasurecoding"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/manifest"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

// Mode is the device class a document names its devices in.
const (
	ModeNVMe  = "nvme"
	ModeBlock = "block"
)

// Request is everything the editor chose.
type Request struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Mode        string `json:"mode"`
	ClusterRef  string `json:"clusterRef,omitempty"`
	Environment string `json:"environment,omitempty"`

	// FromCDC builds on the bundle's CDC, so the fields this editor does not
	// show survive, whether the result keeps its name or is a copy of an
	// approved one under another.
	FromCDC bool `json:"fromCDC,omitempty"`

	Workers []Worker `json:"workers"`

	// Cluster is the cluster the document creates, and is ignored when
	// ClusterRef names one to grow.
	Cluster Cluster `json:"cluster"`
}

// Worker is the choices for one worker.
type Worker struct {
	Name           string   `json:"name"`
	NodeSet        string   `json:"nodeSet"`
	FailureDomain  string   `json:"failureDomain,omitempty"`
	MgmtInterface  string   `json:"mgmtInterface,omitempty"`
	DataInterfaces []string `json:"dataInterfaces,omitempty"`
	Devices        []string `json:"devices"`

	// NetworkAttachment attaches the worker's storage node through
	// NetworkAttachmentDefinitions in place of the interfaces above.
	NetworkAttachment *simplyblockv1alpha2.NetworkAttachmentSelection `json:"networkAttachment,omitempty"`

	// Mode is the device class this worker's devices are named in, and empty
	// for the request's own. A document may name NVMe devices in one node set
	// and block devices in another, and an edit keeps each.
	Mode string `json:"mode,omitempty"`
}

// Cluster is the cluster settings the editor shows.
type Cluster struct {
	Name                     string   `json:"name,omitempty"`
	DataChunks               *int32   `json:"dataChunks,omitempty"`
	ParityChunks             *int32   `json:"parityChunks,omitempty"`
	ClusterMode              string   `json:"clusterMode,omitempty"`
	VCPUCount                *int32   `json:"vcpuCount,omitempty"`
	MaxSubsystemCount        *int32   `json:"maxSubsystemCount,omitempty"`
	MinHugePagesSize         string   `json:"minHugePagesSize,omitempty"`
	NodesPerSocket           *int32   `json:"nodesPerSocket,omitempty"`
	SocketsToUse             []string `json:"socketsToUse,omitempty"`
	EnableDriveFormat        *bool    `json:"enableDriveFormat,omitempty"`
	EnableJournalDevice      *bool    `json:"enableJournalDevice,omitempty"`
	EnableFailureDomains     *bool    `json:"enableFailureDomains,omitempty"`
	EnableChecksumValidation *bool    `json:"enableChecksumValidation,omitempty"`
	EnableAtomicity4K        *bool    `json:"enableAtomicity4K,omitempty"`

	// EnableCpuTopology and EnableKubeletConfiguration are stated only on a
	// Vanilla document; every other environment decides both, and Build drops
	// them there rather than writing a document the schema refuses.
	EnableCpuTopology          *bool `json:"enableCpuTopology,omitempty"`
	EnableKubeletConfiguration *bool `json:"enableKubeletConfiguration,omitempty"`

	// ForceJournalDevice dedicates a journal device even where a worker's
	// smallest devices tie, as spec.discover.forceJournalDevice does for a run.
	// A document has no such field: it writes enableJournalDevice, and the
	// editor stops warning about the tie.
	ForceJournalDevice bool `json:"forceJournalDevice,omitempty"`
}

// Environments are the values spec.environment takes, in the order the editor
// offers them.
var Environments = []simplyblockv1alpha2.KubernetesEnvironment{
	simplyblockv1alpha2.KubernetesEnvironmentVanilla,
	simplyblockv1alpha2.KubernetesEnvironmentOpenShift,
	simplyblockv1alpha2.KubernetesEnvironmentRancher,
	simplyblockv1alpha2.KubernetesEnvironmentK3s,
	simplyblockv1alpha2.KubernetesEnvironmentTalos,
	simplyblockv1alpha2.KubernetesEnvironmentEKS,
	simplyblockv1alpha2.KubernetesEnvironmentGKE,
	simplyblockv1alpha2.KubernetesEnvironmentAKS,
	simplyblockv1alpha2.KubernetesEnvironmentOKE,
}

// defaultNodeSet is the node set a discovery run drafts into.
const defaultNodeSet = "discovered"

// Prefill is the request that rebuilds the bundle's CDC, or, without one, the
// request for what a discovery run would draft from the bundle's reports. An
// approved CDC is immutable, so its prefill carries another name.
func Prefill(b *supportbundle.Bundle) Request {
	req := prefillFromReports(b)
	if b.CDC != nil {
		req = prefillFromCDC(b)
	}
	// A list the editor iterates, also when nothing qualifies: a fleet whose
	// every disk was refused still has workers to choose devices on.
	if req.Workers == nil {
		req.Workers = []Worker{}
	}
	return req
}

func prefillFromCDC(b *supportbundle.Bundle) Request {
	cdc := b.CDC
	req := Request{
		Name:        cdc.Name,
		Namespace:   cdc.Namespace,
		Mode:        ModeNVMe,
		ClusterRef:  cdc.Spec.ClusterRef,
		Environment: string(cdc.Spec.Environment),
		FromCDC:     true,
	}
	if cdc.Spec.Approved {
		req.Name = cdc.Name + "-edit"
	}
	first := true
	for _, set := range cdc.Spec.NodeSets {
		for _, g := range set.Groups {
			mode, devices := ModeNVMe, []string(nil)
			if g.Devices != nil {
				devices = g.Devices.NVMe
				if len(g.Devices.Block) > 0 {
					mode, devices = ModeBlock, g.Devices.Block
				}
			}
			if first {
				req.Mode, first = mode, false
			}
			for _, worker := range g.Workers {
				req.Workers = append(req.Workers, Worker{
					Name: worker, NodeSet: set.Name, FailureDomain: g.FailureDomain,
					MgmtInterface: g.MgmtInterface, DataInterfaces: slices.Clone(g.DataInterfaces),
					Devices: slices.Clone(devices), Mode: mode, NetworkAttachment: g.NetworkAttachment.DeepCopy(),
				})
			}
		}
	}
	if c := cdc.Spec.Cluster; c != nil {
		req.Cluster = Cluster{
			Name: c.Name, ClusterMode: string(c.ClusterMode),
			VCPUCount: c.VCPUCount, MaxSubsystemCount: c.MaxSubsystemCount, MinHugePagesSize: c.MinHugePagesSize,
			NodesPerSocket: c.NodesPerSocket, SocketsToUse: slices.Clone(c.SocketsToUse),
			EnableDriveFormat: c.EnableDriveFormat, EnableJournalDevice: c.EnableJournalDevice,
			EnableFailureDomains: c.EnableFailureDomains, EnableChecksumValidation: c.EnableChecksumValidation,
			EnableAtomicity4K: c.EnableAtomicity4K,
			EnableCpuTopology: c.EnableCpuTopology, EnableKubeletConfiguration: c.EnableKubeletConfiguration,
		}
		if c.Stripe != nil {
			req.Cluster.DataChunks, req.Cluster.ParityChunks = c.Stripe.DataChunks, c.Stripe.ParityChunks
		}
		if cdc.Spec.Approved && c.Name != "" {
			req.Cluster.Name = req.Name + "-cluster"
		}
	}
	return req
}

func prefillFromReports(b *supportbundle.Bundle) Request {
	req := Request{Name: "discovered", Namespace: "simplyblock", Mode: ModeBlock}
	failureDomainLabel := corev1.LabelTopologyZone
	if b.Run != nil {
		req.Name = "discovered-" + b.Run.Name
		req.Namespace = b.Run.Namespace
		req.Environment = string(b.Run.Status.Environment)
		if d := b.Run.Spec.Discover; d != nil && d.FailureDomainLabel != "" {
			failureDomainLabel = d.FailureDomainLabel
		}
	}
	nodes := map[string]corev1.Node{}
	for _, n := range b.Nodes {
		nodes[n.Name] = n
	}

	// The run's own mode, which holds even when it refused every disk. Without
	// a run, the fleet's available disks decide.
	reports := readable(b)
	switch {
	case b.Run != nil:
		req.Mode = ModeNVMe
		if d := b.Run.Spec.Discover; d != nil && d.EnableLogicalBlockDevices != nil && *d.EnableLogicalBlockDevices {
			req.Mode = ModeBlock
		}
	default:
		for _, r := range reports {
			if len(available(r, ModeNVMe)) > 0 {
				req.Mode = ModeNVMe
				break
			}
		}
	}
	// The run's attachment, by the rule the run applies: a cluster without
	// Multus keeps host networking.
	var run *simplyblockv1alpha2.DiscoverSpec
	if b.Run != nil {
		run = b.Run.Spec.Discover
	}
	attachment, _ := discovery.NetworkingOf(run, reports)

	devicesPerWorker := 0
	var vcpus int32
	for _, r := range reports {
		devices := available(r, req.Mode)
		if len(devices) == 0 {
			continue
		}
		node := nodes[r.Node]
		worker := Worker{
			Name: r.Node, NodeSet: defaultNodeSet, FailureDomain: node.Labels[failureDomainLabel],
			Devices: devices, NetworkAttachment: attachment.DeepCopy(),
		}
		if attachment == nil {
			mgmt, data := discovery.Planes(r, discovery.KubeNodeOf(node).InternalIP)
			worker.MgmtInterface, worker.DataInterfaces = mgmt.Name, data
		}
		req.Workers = append(req.Workers, worker)
		if devicesPerWorker == 0 || len(devices) < devicesPerWorker {
			devicesPerWorker = len(devices)
		}
		if cores := chosenNodeCores(r, req.Mode, devices); cores > 0 && (vcpus == 0 || cores < vcpus) {
			vcpus = cores
		}
	}

	on := true
	thirty := int32(30)
	vcpus = max(vcpus, minimumVCPUCount)
	req.Cluster = Cluster{Name: req.Name + "-cluster", EnableDriveFormat: &on, MaxSubsystemCount: &thirty, VCPUCount: &vcpus}
	// 2+1 where the workers carry it, 1+1 where they are too few for 2+1's
	// spare: the operator's own minimums, which a proposal it refuses would
	// only send back.
	data, parity := int32(2), int32(1)
	switch {
	case len(req.Workers) >= 3 && len(req.Workers) < (erasurecoding.Scheme{DataChunks: 2, ParityChunks: 1}).MinimumNodes():
		data = 1
	case len(req.Workers) == 2:
		data, req.Cluster.ClusterMode = 1, string(simplyblockv1alpha2.ClusterModeDual)
	case len(req.Workers) == 1:
		// A single node places a stripe across its devices: 2+1 from three,
		// 1+1 from two, and 1+0 from one, which protects nothing.
		req.Cluster.ClusterMode = string(simplyblockv1alpha2.ClusterModeSingle)
		switch {
		case devicesPerWorker == 2:
			data = 1
		case devicesPerWorker < 2:
			data, parity = 1, 0
		}
	}
	req.Cluster.DataChunks, req.Cluster.ParityChunks = &data, &parity
	return req
}

// readable is the bundle's readable reports, by worker name.
func readable(b *supportbundle.Bundle) []nodeprobe.Report {
	var out []nodeprobe.Report
	for _, r := range b.Reports {
		if r.Err == "" && r.Report.Node != "" {
			out = append(out, r.Report)
		}
	}
	slices.SortFunc(out, func(x, y nodeprobe.Report) int { return strings.Compare(x.Node, y.Node) })
	return out
}

// available is the names of a report's available devices in a mode.
func available(r nodeprobe.Report, mode string) []string {
	var out []string
	for _, d := range r.Devices {
		if !d.Available || d.Kind != "Disk" {
			continue
		}
		switch {
		case mode == ModeNVMe && d.Transport == "NVMe" && d.PCIAddress != "":
			out = append(out, d.PCIAddress)
		case mode == ModeBlock && d.Transport != "NVMeFabric":
			out = append(out, cmpOr(d.StablePath, d.Path))
		}
	}
	// A controller with several namespaces is one PCI address, which a device
	// list names once.
	slices.Sort(out)
	return slices.Compact(out)
}

// minimumVCPUCount is the API's floor for vcpuCount.
const minimumVCPUCount int32 = 4

// chosenNodeCores is the physical cores of the memory node a worker's chosen
// devices hang off, which is how a discovery run sizes vcpuCount. Devices on
// more than one node, or on a node the CPU reading does not list, give none.
func chosenNodeCores(r nodeprobe.Report, mode string, chosen []string) int32 {
	node, found := 0, false
	for _, d := range r.Devices {
		id := d.PCIAddress
		if mode == ModeBlock {
			id = cmpOr(d.StablePath, d.Path)
		}
		if !slices.Contains(chosen, id) {
			continue
		}
		if found && d.NUMANode != node {
			return 0
		}
		node, found = d.NUMANode, true
	}
	if !found {
		return 0
	}
	for _, cpus := range r.CPU.NUMANodes {
		if cpus.Node == node {
			return int32(cpus.PhysicalCores)
		}
	}
	return 0
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Build turns a request into a document. It fails for a request no document can
// state, and leaves every other problem to the checks the caller runs on the
// result.
func Build(b *supportbundle.Bundle, req Request) (*simplyblockv1alpha2.ClusterDeploymentConfig, error) {
	if err := validate(req); err != nil {
		return nil, err
	}

	if b.CDC != nil && b.CDC.Name == req.Name && b.CDC.Spec.Approved {
		return nil, fmt.Errorf("%s is approved, and an approved document cannot be changed: "+
			"give the new document another name", req.Name)
	}
	cdc := &simplyblockv1alpha2.ClusterDeploymentConfig{}
	if b.CDC != nil && (req.FromCDC || b.CDC.Name == req.Name) {
		cdc = b.CDC.DeepCopy()
	}
	cdc.ObjectMeta = metav1.ObjectMeta{
		Name: req.Name, Namespace: req.Namespace,
		Labels: cdc.Labels, Annotations: cdc.Annotations,
	}
	cdc.Status = simplyblockv1alpha2.ClusterDeploymentConfigStatus{}
	cdc.Spec.Approved = false
	cdc.Spec.ClusterRef = req.ClusterRef
	if req.Environment != "" {
		cdc.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironment(req.Environment)
	}

	// A document growing a supercluster creates its new members, and states
	// their sizing in spec.cluster. One growing an ordinary cluster states none.
	if req.ClusterRef != "" && !isSupercluster(b, req.ClusterRef) {
		cdc.Spec.Cluster = nil
	} else {
		cdc.Spec.Cluster = clusterOf(cdc.Spec.Cluster, req)
		var missing []string
		if cdc.Spec.Cluster.VCPUCount == nil {
			missing = append(missing, fmt.Sprintf("a vCPU count (at least %d)", minimumVCPUCount))
		}
		if cdc.Spec.Cluster.MaxSubsystemCount == nil {
			missing = append(missing, "a max subsystem count (at least 10)")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("the cluster needs %s", strings.Join(missing, " and "))
		}
	}
	cdc.Spec.NodeSets = nodeSetsOf(b, cdc.Spec.NodeSets, req)
	return cdc, nil
}

// isSupercluster reports whether the bundle shows the named cluster as a
// supercluster.
func isSupercluster(b *supportbundle.Bundle, name string) bool {
	for _, c := range b.StorageClusters {
		if c.Name == name {
			return c.Spec.Supercluster != nil || c.Status.Role == simplyblockv1alpha2.StorageClusterRoleSupercluster
		}
	}
	return false
}

func validate(req Request) error {
	var problems []string
	if req.Name == "" || req.Namespace == "" {
		problems = append(problems, "the document needs a name and a namespace")
	}
	if req.Mode != ModeNVMe && req.Mode != ModeBlock {
		problems = append(problems, fmt.Sprintf("the mode is %q, and is nvme or block", req.Mode))
	}
	if len(req.Workers) == 0 {
		problems = append(problems, "no worker is chosen")
	}
	seen := map[string]bool{}
	for _, w := range req.Workers {
		if w.Mode != "" && w.Mode != ModeNVMe && w.Mode != ModeBlock {
			problems = append(problems, fmt.Sprintf("%s's mode is %q, and is nvme or block", w.Name, w.Mode))
		}
		if seen[w.Name] {
			problems = append(problems, w.Name+" is chosen twice")
		}
		seen[w.Name] = true
		if len(w.Devices) == 0 {
			problems = append(problems, w.Name+" hands over no device")
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// clusterOf sets the editor's fields on the cluster the document already
// states, so the fields it does not show stay as they are.
func clusterOf(base *simplyblockv1alpha2.ClusterTemplate, req Request) *simplyblockv1alpha2.ClusterTemplate {
	c := &simplyblockv1alpha2.ClusterTemplate{}
	if base != nil {
		c = base.DeepCopy()
	}
	e := req.Cluster
	c.Name = cmpOr(e.Name, req.Name+"-cluster")
	c.ClusterMode = simplyblockv1alpha2.ClusterMode(e.ClusterMode)
	c.VCPUCount, c.MaxSubsystemCount, c.NodesPerSocket = e.VCPUCount, e.MaxSubsystemCount, e.NodesPerSocket
	c.MinHugePagesSize, c.SocketsToUse = e.MinHugePagesSize, e.SocketsToUse
	c.EnableDriveFormat, c.EnableJournalDevice = e.EnableDriveFormat, e.EnableJournalDevice
	c.EnableFailureDomains, c.EnableChecksumValidation = e.EnableFailureDomains, e.EnableChecksumValidation
	c.EnableAtomicity4K = e.EnableAtomicity4K
	if e.ForceJournalDevice {
		c.EnableJournalDevice = ptr.To(true)
	}
	c.EnableCpuTopology, c.EnableKubeletConfiguration = nil, nil
	if req.Environment == string(simplyblockv1alpha2.KubernetesEnvironmentVanilla) {
		c.EnableCpuTopology, c.EnableKubeletConfiguration = e.EnableCpuTopology, e.EnableKubeletConfiguration
	}
	c.Stripe = nil
	if e.DataChunks != nil || e.ParityChunks != nil {
		c.Stripe = &simplyblockv1alpha2.StripeSpec{DataChunks: e.DataChunks, ParityChunks: e.ParityChunks}
	}
	return c
}

// nodeSetsOf groups the workers. Node sets and groups keep the order of their
// first worker. A group's settings the editor does not show carry over from
// the group its workers were in, when they were all in the same one, and a node
// set's cluster overrides carry over by name.
func nodeSetsOf(
	b *supportbundle.Bundle, base []simplyblockv1alpha2.NodeSet, req Request,
) []simplyblockv1alpha2.NodeSet {
	baseSet := map[string]simplyblockv1alpha2.NodeSet{}
	baseGroup := map[string]*simplyblockv1alpha2.NodeGroup{}
	for i := range base {
		baseSet[base[i].Name] = base[i]
		for j := range base[i].Groups {
			for _, w := range base[i].Groups[j].Workers {
				baseGroup[w] = &base[i].Groups[j]
			}
		}
	}
	reports := map[string]nodeprobe.Report{}
	for _, r := range readable(b) {
		reports[r.Node] = r
	}

	var sets []simplyblockv1alpha2.NodeSet
	setIndex := map[string]int{}
	groupIndex := map[string]int{}
	sources := map[string]*simplyblockv1alpha2.NodeGroup{}
	for _, w := range req.Workers {
		setName := cmpOr(w.NodeSet, defaultNodeSet)
		si, ok := setIndex[setName]
		if !ok {
			si = len(sets)
			setIndex[setName] = si
			sets = append(sets, simplyblockv1alpha2.NodeSet{Name: setName, Cluster: baseSet[setName].Cluster})
		}
		mode := cmpOr(w.Mode, req.Mode)
		devices := slices.Compact(slices.Sorted(slices.Values(w.Devices)))
		// The settings the editor does not show are part of what makes two
		// workers one group: workers whose groups state different ones stay
		// apart, and those whose groups state the same ones keep them.
		key := strings.Join([]string{setName, mode, strings.Join(devices, ","), w.MgmtInterface,
			strings.Join(w.DataInterfaces, ","), attachmentKey(w.NetworkAttachment), w.FailureDomain,
			hiddenSettings(baseGroup[w.Name])}, "|")
		if gi, ok := groupIndex[key]; ok {
			g := &sets[si].Groups[gi]
			g.Workers = append(g.Workers, w.Name)
			continue
		}
		g := simplyblockv1alpha2.NodeGroup{
			Name:    groupName(len(sets[si].Groups)+1, mode, devices, reports[w.Name]),
			Workers: []string{w.Name}, MgmtInterface: w.MgmtInterface,
			DataInterfaces: w.DataInterfaces, FailureDomain: w.FailureDomain,
			NetworkAttachment: w.NetworkAttachment.DeepCopy(),
			Devices:           &simplyblockv1alpha2.DeviceSelection{},
		}
		if mode == ModeNVMe {
			g.Devices.NVMe = devices
		} else {
			g.Devices.Block = devices
		}
		groupIndex[key] = len(sets[si].Groups)
		sources[key] = baseGroup[w.Name]
		sets[si].Groups = append(sets[si].Groups, g)
	}
	for key, src := range sources {
		if src == nil {
			continue
		}
		set := strings.SplitN(key, "|", 2)[0]
		g := &sets[setIndex[set]].Groups[groupIndex[key]]
		g.SpdkSystemMemory, g.ReservedSystemCPU = src.SpdkSystemMemory, src.ReservedSystemCPU
		g.JournalManager = src.JournalManager
	}
	return sets
}

// hiddenSettings is a group's settings the editor does not show, as one string
// to compare, and empty for a worker no group of the document names.
func hiddenSettings(g *simplyblockv1alpha2.NodeGroup) string {
	if g == nil {
		return ""
	}
	out, _ := json.Marshal(struct {
		SpdkSystemMemory  string
		ReservedSystemCPU string
		JournalManager    *simplyblockv1alpha2.JournalManagerSpec
	}{g.SpdkSystemMemory, g.ReservedSystemCPU, g.JournalManager})
	return string(out)
}

// groupName follows the discovery run's pattern: group-<n>-<class>-<count>x<size>.
func groupName(n int, mode string, devices []string, report nodeprobe.Report) string {
	size := ""
	for _, d := range report.Devices {
		if d.PCIAddress == devices[0] || d.StablePath == devices[0] || d.Path == devices[0] {
			size = humanSize(d.SizeBytes)
			break
		}
	}
	if size == "" {
		return fmt.Sprintf("group-%d-%s-%d", n, mode, len(devices))
	}
	return fmt.Sprintf("group-%d-%s-%dx%s", n, mode, len(devices), size)
}

func humanSize(bytes uint64) string {
	const gib, tib = 1 << 30, 1 << 40
	if bytes >= tib {
		return fmt.Sprintf("%dT", (bytes+tib/2)/tib)
	}
	return fmt.Sprintf("%dG", (bytes+gib/2)/gib)
}

// Render is a document as YAML to apply: no status and no server-set metadata.
func Render(cdc *simplyblockv1alpha2.ClusterDeploymentConfig) ([]byte, error) {
	return manifest.Render(cdc, simplyblockv1alpha2.GroupVersion.String(), "ClusterDeploymentConfig")
}

// attachmentKey is a worker's attachment as part of its group key.
func attachmentKey(attachment *simplyblockv1alpha2.NetworkAttachmentSelection) string {
	if attachment == nil {
		return ""
	}
	return attachment.Management + "/" + attachment.Data
}
