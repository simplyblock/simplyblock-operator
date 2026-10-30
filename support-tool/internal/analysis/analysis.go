// What a support agent reads off a bundle: the run, every worker with its
// facts and components, the cluster objects the bundle captured, and the
// ClusterDeploymentConfig with everything wrong with it.
//
// It is a view model, built once per bundle and sent to the browser as JSON. The
// probe report's own types are passed through rather than copied, so the page
// shows what the operator would read, and the interface choice is the
// discovery package's own, so a suggestion here is what a run would draft.

package analysis

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/discovery"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

// Analysis is everything the tool shows about one bundle.
type Analysis struct {
	Bundle string `json:"bundle"`

	// Diagnoses are the bundle's problems, errors first: what the page opens
	// with, so the reason a discovery went wrong is the first thing read.
	Diagnoses []Diagnosis `json:"diagnoses"`

	Run            *Run     `json:"run,omitempty"`
	Workers        []Worker `json:"workers"`
	Cluster        Cluster  `json:"cluster"`
	CDC            *CDC     `json:"cdc,omitempty"`
	Problems       []string `json:"problems,omitempty"`
	Collection     string   `json:"collection,omitempty"`
	WarningEvents  []string `json:"warningEvents,omitempty"`
	KubectlVersion string   `json:"kubectlVersion,omitempty"`
}

// Run is the discovery run the bundle was collected for.
type Run struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Action      string `json:"action"`
	Phase       string `json:"phase,omitempty"`
	Message     string `json:"message,omitempty"`
	Environment string `json:"environment,omitempty"`
	Created     string `json:"created,omitempty"`
	ConfigRef   string `json:"configRef,omitempty"`
	// Settings is the run's spec.discover as YAML, empty for the defaults.
	Settings string `json:"settings,omitempty"`
}

// Worker is one machine, as Kubernetes and its probe report describe it.
type Worker struct {
	Name string `json:"name"`

	// InCluster reports whether the bundle carries the Node object, and
	// InReport whether it carries a probe report for it.
	InCluster bool `json:"inCluster"`
	InReport  bool `json:"inReport"`

	ReportFile    string `json:"reportFile,omitempty"`
	ReportVersion int    `json:"reportVersion,omitempty"`
	ReportError   string `json:"reportError,omitempty"`

	// KernelReported is false for a report version that predates the kernel
	// facts, whose empty kernel section is no reading at all.
	KernelReported bool `json:"kernelReported"`

	// NetworkAttachmentsReported is false for a report version that predates
	// the NetworkAttachmentDefinitions, whose absent reading says nothing about
	// whether the cluster runs Multus.
	NetworkAttachmentsReported bool `json:"networkAttachmentsReported"`

	Node   Node              `json:"node"`
	Report *nodeprobe.Report `json:"report,omitempty"`

	// Devices are the report's devices with the name a CDC selects each by.
	Devices []Device `json:"devices,omitempty"`

	// Suggested is what a discovery run would pick for this worker's
	// management and data interfaces.
	Suggested Interfaces `json:"suggested"`

	// StorageNodes are the StorageNodes the bundle shows on this worker.
	StorageNodes []string `json:"storageNodes,omitempty"`

	Summary Summary `json:"summary"`
}

// Node is what the Kubernetes Node object says.
type Node struct {
	Roles          []string          `json:"roles,omitempty"`
	InternalIP     string            `json:"internalIP,omitempty"`
	Cordoned       bool              `json:"cordoned"`
	Taints         []string          `json:"taints,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Ready          string            `json:"ready,omitempty"`
	KubeletVersion string            `json:"kubeletVersion,omitempty"`
	OSImage        string            `json:"osImage,omitempty"`
}

// Device is a probe report device with the names a CDC uses for it.
type Device struct {
	nodeprobe.Device

	// NVMeID is the PCI address a CDC in NVMe mode names it by, and BlockID
	// the path one in logical block device mode does. Either is empty where
	// the device has none.
	NVMeID  string `json:"nvmeID,omitempty"`
	BlockID string `json:"blockID,omitempty"`
}

// Interfaces is a management interface and data interfaces.
type Interfaces struct {
	Management string   `json:"management,omitempty"`
	Data       []string `json:"data,omitempty"`
}

// Summary is a worker's line in the overview.
type Summary struct {
	Devices        int    `json:"devices"`
	Available      int    `json:"available"`
	AvailableBytes uint64 `json:"availableBytes"`
	Interfaces     int    `json:"interfaces"`
	Controllers    int    `json:"controllers"`
}

// Cluster is the simplyblock objects the bundle captured.
type Cluster struct {
	ControlPlanes   []Object `json:"controlPlanes,omitempty"`
	StorageClusters []Object `json:"storageClusters,omitempty"`
	StorageNodes    []Object `json:"storageNodes,omitempty"`
}

// Object is one of them.
type Object struct {
	Name  string `json:"name"`
	Phase string `json:"phase,omitempty"`
	// Role is a StorageCluster's: Standalone, Supercluster, or PlacementGroup
	// for a member, and Supercluster names a member's supercluster.
	Role         string `json:"role,omitempty"`
	Supercluster string `json:"supercluster,omitempty"`
	Worker       string `json:"worker,omitempty"`
	Cluster      string `json:"cluster,omitempty"`
}

// CDC is the bundle's ClusterDeploymentConfig and what is wrong with it.
type CDC struct {
	Name     string    `json:"name"`
	Approved bool      `json:"approved"`
	Phase    string    `json:"phase,omitempty"`
	Message  string    `json:"message,omitempty"`
	YAML     string    `json:"yaml"`
	NodeSets []NodeSet `json:"nodeSets"`
	Cluster  string    `json:"cluster,omitempty"`
	Findings []Finding `json:"findings"`
}

// NodeSet is one node set of a CDC.
type NodeSet struct {
	Name   string  `json:"name"`
	Groups []Group `json:"groups"`
}

// Group is one group of a node set.
type Group struct {
	Name           string   `json:"name"`
	Workers        []string `json:"workers"`
	Class          string   `json:"class"`
	Devices        []string `json:"devices"`
	MgmtInterface  string   `json:"mgmtInterface,omitempty"`
	DataInterfaces []string `json:"dataInterfaces,omitempty"`
	FailureDomain  string   `json:"failureDomain,omitempty"`

	// NetworkAttachment is what the group attaches its storage nodes through
	// instead of host interfaces, nil on host networking.
	NetworkAttachment *simplyblockv1alpha2.NetworkAttachmentSelection `json:"networkAttachment,omitempty"`
}

// Severity says whether a finding keeps a document from deploying.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Finding is one problem with a CDC. Source is "operator" for the checks the
// operator runs on a draft, and "bundle" for those only the bundle's reports
// can answer.
type Finding struct {
	Source   string   `json:"source"`
	Severity Severity `json:"severity"`
	Reason   string   `json:"reason"`
	Message  string   `json:"message"`
}

// Analyze builds the view of a bundle.
func Analyze(b *supportbundle.Bundle) Analysis {
	a := Analysis{
		Bundle:         b.Name,
		Run:            runOf(b),
		Workers:        workersOf(b),
		Cluster:        clusterOf(b),
		Problems:       append([]string(nil), b.Problems...),
		Collection:     strings.TrimSpace(b.Errors),
		KubectlVersion: strings.TrimSpace(b.KubectlVersion),
	}
	for _, r := range b.Reports {
		if r.Err != "" {
			a.Problems = append(a.Problems, "reports/"+r.File+": "+r.Err)
		}
	}
	for _, line := range strings.Split(b.Events, "\n") {
		if strings.Contains(line, "Warning") {
			a.WarningEvents = append(a.WarningEvents, strings.TrimSpace(line))
		}
	}
	if b.CDC != nil {
		a.CDC = cdcOf(b)
	}
	a.Diagnoses = diagnose(b, a)
	return a
}

func runOf(b *supportbundle.Bundle) *Run {
	if b.Run == nil {
		return nil
	}
	ops := b.Run
	run := &Run{
		Name:        ops.Name,
		Namespace:   ops.Namespace,
		Action:      string(ops.Spec.Action),
		Phase:       string(ops.Status.Phase),
		Message:     ops.Status.Message,
		Environment: string(ops.Status.Environment),
		ConfigRef:   ops.Status.ConfigRef,
	}
	if !ops.CreationTimestamp.IsZero() {
		run.Created = ops.CreationTimestamp.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	if ops.Spec.Discover != nil {
		if out, err := yaml.Marshal(ops.Spec.Discover); err == nil && strings.TrimSpace(string(out)) != "{}" {
			run.Settings = string(out)
		}
	}
	return run
}

// workersOf joins the Node objects and the probe reports by worker name. A
// worker the run selected but never reported on is still a worker, and a
// report for a node the bundle does not carry is still a report.
func workersOf(b *supportbundle.Bundle) []Worker {
	byName := map[string]*Worker{}
	get := func(name string) *Worker {
		if w, ok := byName[name]; ok {
			return w
		}
		w := &Worker{Name: name}
		byName[name] = w
		return w
	}

	// The run's own selection first: a worker it selected is part of the story
	// even when its Node object and its report are both missing from the bundle.
	if b.Run != nil {
		for _, name := range b.Run.Status.Workers {
			get(name)
		}
	}
	for _, node := range b.Nodes {
		w := get(node.Name)
		w.InCluster = true
		w.Node = nodeOf(node)
	}
	for i := range b.Reports {
		r := b.Reports[i]
		if r.Err != "" || r.Report.Node == "" {
			continue
		}
		w := get(r.Report.Node)
		w.InReport = true
		w.ReportFile = r.File
		w.ReportVersion = r.Version
		w.KernelReported = r.KernelReported()
		w.NetworkAttachmentsReported = r.NetworkAttachmentsReported()
		report := r.Report
		w.Report = &report
	}
	for _, sn := range b.StorageNodes {
		if sn.Spec.WorkerNode != "" {
			w := get(sn.Spec.WorkerNode)
			w.StorageNodes = append(w.StorageNodes, sn.Name)
		}
	}

	out := make([]Worker, 0, len(byName))
	for _, w := range byName {
		if w.Report != nil {
			w.Devices = devicesOf(*w.Report)
			mgmt, data := discovery.Planes(*w.Report, w.Node.InternalIP)
			w.Suggested = Interfaces{Management: mgmt.Name, Data: data}
			w.Summary = summaryOf(*w.Report)
		}
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func nodeOf(node corev1.Node) Node {
	kube := discovery.KubeNodeOf(node)
	n := Node{
		InternalIP:     kube.InternalIP,
		Cordoned:       kube.Unschedulable,
		Taints:         kube.Taints,
		Labels:         kube.Labels,
		KubeletVersion: node.Status.NodeInfo.KubeletVersion,
		OSImage:        node.Status.NodeInfo.OSImage,
	}
	for _, role := range kube.Roles {
		n.Roles = append(n.Roles, string(role))
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			n.Ready = string(c.Status)
		}
	}
	return n
}

// devicesOf names each device the way a CDC selects it: by PCI address in NVMe
// mode, and by its stable path, or its kernel path without one, in logical
// block device mode.
func devicesOf(report nodeprobe.Report) []Device {
	out := make([]Device, 0, len(report.Devices))
	for _, d := range report.Devices {
		dev := Device{Device: d}
		if d.Kind == "Disk" && d.Transport == "NVMe" && d.PCIAddress != "" {
			dev.NVMeID = d.PCIAddress
		}
		if d.StablePath != "" {
			dev.BlockID = d.StablePath
		} else {
			dev.BlockID = d.Path
		}
		out = append(out, dev)
	}
	return out
}

func summaryOf(report nodeprobe.Report) Summary {
	s := Summary{
		Devices:     len(report.Devices),
		Interfaces:  len(report.Interfaces),
		Controllers: len(report.NVMeControllers),
	}
	for _, d := range report.Devices {
		if d.Available {
			s.Available++
			s.AvailableBytes += d.SizeBytes
		}
	}
	return s
}

func clusterOf(b *supportbundle.Bundle) Cluster {
	var c Cluster
	for _, o := range b.ControlPlanes {
		c.ControlPlanes = append(c.ControlPlanes, Object{Name: o.Name, Phase: string(o.Status.Phase)})
	}
	for _, o := range b.StorageClusters {
		c.StorageClusters = append(c.StorageClusters, Object{
			Name: o.Name, Phase: string(o.Status.Phase), Role: roleOf(o), Supercluster: o.Spec.SuperclusterRef,
		})
	}
	for _, o := range b.StorageNodes {
		c.StorageNodes = append(c.StorageNodes, Object{
			Name: o.Name, Phase: string(o.Status.Phase), Worker: o.Spec.WorkerNode, Cluster: o.Spec.ClusterRef,
		})
	}
	return c
}

// roleOf is a cluster's role as its status records it, or as its spec implies
// for a cluster whose status predates the field.
func roleOf(c simplyblockv1alpha2.StorageCluster) string {
	switch {
	case c.Status.Role != "":
		return string(c.Status.Role)
	case c.Spec.Supercluster != nil:
		return string(simplyblockv1alpha2.StorageClusterRoleSupercluster)
	case c.Spec.SuperclusterRef != "":
		return string(simplyblockv1alpha2.StorageClusterRolePlacementGroup)
	}
	return string(simplyblockv1alpha2.StorageClusterRoleStandalone)
}
