// The vocabulary of a takeover: what a pass observes about the guests, and the
// small interfaces it acts through.
//
// The interfaces are narrow on purpose. The decisions in this package are
// tested with fakes of each, and the Kubernetes, csi-link, and EndpointSlice
// implementations live where those clients already are.

package mdsfailover

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

// Guest is one metadata server pod as a pass sees it.
type Guest struct {
	Pod     string
	Ordinal int
	Node    string
	// Zone is the node's topology.kubernetes.io/zone, empty without the label.
	Zone string
	UID  string
	// Instance names one boot of the guest, in the assembledBy format.
	Instance string
	PodIP    string
	// Revision is the pod's controller-revision-hash.
	Revision  string
	Ready     bool
	NodeReady bool
	Deleting  bool
	// HasSession is whether a csi-link session of the pod exists now.
	HasSession bool
	// Role and Epoch are the guest's answer to Describe, and zero when it has no
	// session or did not answer.
	Role  nfsexportrpc.Role
	Epoch uint64
}

// Observation is everything one pass decides from.
type Observation struct {
	// Cluster is the storage cluster the metadata server serves.
	Cluster string
	// StatefulSet is the workload's name, and StatefulSetUID seeds the keys.
	StatefulSet    string
	StatefulSetUID string
	// Version is the StatefulSet's resourceVersion, which the record write is
	// conditioned on.
	Version string
	Record  Record
	// StateVolume is the state volume's handle, empty until the workload has
	// recorded one.
	StateVolume    string
	Replicas       int
	UpdateRevision string
	Guests         []Guest
}

// guest returns the named guest.
func (o Observation) guest(pod string) (Guest, bool) {
	for _, g := range o.Guests {
		if g.Pod == pod {
			return g, true
		}
	}
	return Guest{}, false
}

// Observer reads the cluster. Every API read it makes is retried (§7.2), and an
// error it returns means the pass learned nothing.
type Observer interface {
	Observe(ctx context.Context) (Observation, error)
}

// Guests is the link to the guests' MDSService.
type Guests interface {
	Describe(ctx context.Context, pod string) (nfsexportrpc.Description, error)
	Promote(ctx context.Context, pod string, req nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error)
}

// Gateway moves the metadata server's one Service endpoint.
type Gateway interface {
	// Withdraw empties the EndpointSlice and has the nodes forget the flows to the
	// address it held. Idempotent: an empty slice asks for nothing. pod is a pod of
	// the StatefulSet, which names its Service.
	Withdraw(ctx context.Context, pod string) error
	// Serve points the EndpointSlice at the pod IP, and has the nodes forget the flows to
	// the address withdrawn or replaced. Idempotent.
	Serve(ctx context.Context, pod, ip string) error
}

// ExportState is the part of an export's status a takeover reads.
type ExportState struct {
	Key         types.NamespacedName
	Pod         string
	AssembledBy string
}

// Exports is the cluster's NFSExports.
type Exports interface {
	// List is every export of the storage cluster that is not being deleted.
	List(ctx context.Context) ([]ExportState, error)
	// Specs is the spec of every export the guest can assemble now, for the host
	// NQN given.
	Specs(ctx context.Context, hostNQN string) ([]nfsexport.Spec, error)
	// Rebind rewrites status.mdsPodName to pod, which hands the export to the
	// existing resync.
	Rebind(ctx context.Context, export types.NamespacedName, pod string) error
}

// Pods deletes a pod, for a demotion and for a planned switchover.
type Pods interface {
	Delete(ctx context.Context, pod string) error
}

// Events is where a takeover's events go.
type Events interface {
	// Driver emits on the SimplyblockDriver.
	Driver(eventType, reason, message string)
	// Exports emits on every NFSExport of the cluster.
	Exports(eventType, reason, message string)
}

// Config is the tunable part of a pass.
type Config struct {
	// DetectionWindow is how long the loss conditions hold before a takeover
	// starts (Open Question 1 of the design). Zero takes DefaultDetectionWindow.
	DetectionWindow time.Duration
	// Parallelism is how many exports reassemble at once. Zero takes 8.
	Parallelism int
	// FenceBackoff is the first retry delay of a failed promotion, doubled up to
	// MaxFenceBackoff. Zero takes the defaults.
	FenceBackoff    time.Duration
	MaxFenceBackoff time.Duration
}

const (
	// DefaultDetectionWindow is 20 seconds, which after the node controller's
	// 40-second grace period puts a takeover about 45 seconds after the node's
	// last heartbeat.
	DefaultDetectionWindow = 20 * time.Second
	defaultParallelism     = 8
	defaultFenceBackoff    = 5 * time.Second
	defaultMaxFenceBackoff = 2 * time.Minute
)

func (c Config) window() time.Duration {
	if c.DetectionWindow <= 0 {
		return DefaultDetectionWindow
	}
	return c.DetectionWindow
}

func (c Config) parallelism() int {
	if c.Parallelism <= 0 {
		return defaultParallelism
	}
	return c.Parallelism
}

func (c Config) fenceBackoff() (first, limit time.Duration) {
	first, limit = c.FenceBackoff, c.MaxFenceBackoff
	if first <= 0 {
		first = defaultFenceBackoff
	}
	if limit <= 0 {
		limit = defaultMaxFenceBackoff
	}
	return first, limit
}

// Cause is why a takeover started.
type Cause string

const (
	// CauseFirst is the first promotion of a storage cluster's metadata server.
	CauseFirst Cause = "First"
	// CauseDeleted is a pod with a deletion timestamp, the planned case.
	CauseDeleted Cause = "Deleted"
	// CauseLost is an active that vanished for the detection window.
	CauseLost Cause = "Lost"
)

// Result is what a pass asks of its caller: when to look again, and whether
// anything is in progress.
type Result struct {
	RequeueAfter time.Duration
	// Settled is a cluster whose active serves every export and has nothing
	// pending: no takeover, no rollout step, no wait.
	Settled bool
}
