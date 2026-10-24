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
	"sigs.k8s.io/yaml"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/discovery"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
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
}

// defaultNodeSet is the node set a discovery run drafts into.
const defaultNodeSet = "discovered"

// Prefill is the request that rebuilds the bundle's CDC, or, without one, the
// request for what a discovery run would draft from the bundle's reports. An
// approved CDC is immutable, so its prefill carries another name.
func Prefill(b *supportbundle.Bundle) Request {
	if b.CDC != nil {
		return prefillFromCDC(b)
	}
	return prefillFromReports(b)
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
	for _, set := range cdc.Spec.NodeSets {
		for _, g := range set.Groups {
			devices := []string(nil)
			if g.Devices != nil {
				devices = g.Devices.NVMe
				if len(g.Devices.Block) > 0 {
					req.Mode = ModeBlock
					devices = g.Devices.Block
				}
			}
			for _, worker := range g.Workers {
				req.Workers = append(req.Workers, Worker{
					Name: worker, NodeSet: set.Name, FailureDomain: g.FailureDomain,
					MgmtInterface: g.MgmtInterface, DataInterfaces: slices.Clone(g.DataInterfaces),
					Devices: slices.Clone(devices),
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

	reports := readable(b)
	for _, r := range reports {
		if len(available(r, ModeNVMe)) > 0 {
			req.Mode = ModeNVMe
			break
		}
	}
	devicesPerWorker := 0
	for _, r := range reports {
		devices := available(r, req.Mode)
		if len(devices) == 0 {
			continue
		}
		node := nodes[r.Node]
		mgmt, data := discovery.Planes(r, discovery.KubeNodeOf(node).InternalIP)
		req.Workers = append(req.Workers, Worker{
			Name: r.Node, NodeSet: defaultNodeSet, FailureDomain: node.Labels[failureDomainLabel],
			MgmtInterface: mgmt.Name, DataInterfaces: data, Devices: devices,
		})
		if devicesPerWorker == 0 || len(devices) < devicesPerWorker {
			devicesPerWorker = len(devices)
		}
	}

	on := true
	thirty := int32(30)
	req.Cluster = Cluster{Name: req.Name + "-cluster", EnableDriveFormat: &on, MaxSubsystemCount: &thirty}
	data, parity := int32(2), int32(1)
	switch {
	case len(req.Workers) == 2:
		data, req.Cluster.ClusterMode = 1, string(simplyblockv1alpha2.ClusterModeDual)
	case len(req.Workers) == 1:
		req.Cluster.ClusterMode = string(simplyblockv1alpha2.ClusterModeSingle)
		if devicesPerWorker < 3 {
			data = 1
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
	slices.Sort(out)
	return out
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

	if req.ClusterRef != "" {
		cdc.Spec.Cluster = nil
	} else {
		cdc.Spec.Cluster = clusterOf(cdc.Spec.Cluster, req)
	}
	cdc.Spec.NodeSets = nodeSetsOf(b, cdc.Spec.NodeSets, req)
	return cdc, nil
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
		devices := slices.Sorted(slices.Values(w.Devices))
		key := strings.Join([]string{setName, strings.Join(devices, ","), w.MgmtInterface,
			strings.Join(w.DataInterfaces, ","), w.FailureDomain}, "|")
		if gi, ok := groupIndex[key]; ok {
			g := &sets[si].Groups[gi]
			g.Workers = append(g.Workers, w.Name)
			if sources[key] != baseGroup[w.Name] {
				sources[key] = nil
			}
			continue
		}
		g := simplyblockv1alpha2.NodeGroup{
			Name:    groupName(len(sets[si].Groups)+1, req.Mode, devices, reports[w.Name]),
			Workers: []string{w.Name}, MgmtInterface: w.MgmtInterface,
			DataInterfaces: w.DataInterfaces, FailureDomain: w.FailureDomain,
			Devices: &simplyblockv1alpha2.DeviceSelection{},
		}
		if req.Mode == ModeNVMe {
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
	raw, err := json.Marshal(cdc)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	doc["apiVersion"] = simplyblockv1alpha2.GroupVersion.String()
	doc["kind"] = "ClusterDeploymentConfig"
	delete(doc, "status")
	if meta, ok := doc["metadata"].(map[string]any); ok {
		for _, k := range []string{"resourceVersion", "uid", "creationTimestamp", "managedFields", "generation"} {
			delete(meta, k)
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return yaml.JSONToYAML(out)
}
