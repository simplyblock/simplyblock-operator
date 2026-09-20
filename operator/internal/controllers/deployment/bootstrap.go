// The one discovery run a fresh install performs by itself.
//
// An operator installed into a cluster with disks in it can tell the
// administrator what it found, and that is a better first experience than an
// empty namespace and a document to write by hand. So a fresh install raises one
// OperatorOps with action Discover, whose output is a ClusterDeploymentConfig in
// Draft that nobody has approved and nothing acts on (§8.3).
//
// The whole of the design is the guard. A discovery run is read-only against the
// control plane and the draft it writes is inert, but Probing creates one Job per
// worker, so a run that fired on every restart would put a Job on every node of
// the fleet every time the operator was upgraded. The guard is therefore that no
// previous result exists and there is something to look at, which is four
// questions rather than one:
//
//   - Has any OperatorOps ever run? A terminal one is a previous result, and so
//     is a failed one: the administrator has seen the answer and either acted on
//     it or chose not to, and either way this operator is not the thing to decide
//     they want another.
//   - Does any ClusterDeploymentConfig exist? A document is a previous run's
//     output or somebody's hand-written deployment, and either way discovery has
//     nothing to add that they did not already have.
//   - Does any StorageCluster exist? A deployed fleet was bootstrapped by
//     something, and a fresh install is the only state this exists for.
//   - Is there a machine a run would inspect at all? This one is not about
//     previous results, and it is the question with a cost behind it rather than
//     a convenience. A run raised against a cluster with no usable worker fails,
//     and a failed run is still an object holding a finalizer. Uninstalling the
//     operator deletes its namespace and its Deployment together, so the
//     controller that would release that finalizer can be gone before it sees the
//     delete, and the namespace stays Terminating. An install that raises a run
//     it already knows cannot succeed has made its own uninstall conditional on
//     timing, which is the single-node development cluster exactly: its one
//     machine is the control-plane node.
//
// Any one of the four is enough to decline. Together, they mean the run happens
// on the install that has nothing and has something to look at, and never again.
//
// It runs under leader election, so one replica asks. The create is idempotent by
// name as well, because two replicas answering the same questions at once
// would both conclude yes.

package deployment

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// InitialDiscoveryName is what the run is called. It is fixed rather than
// generated so that the create is idempotent by name on top of the guard below,
// and so that an administrator who does not want it can say so by writing an
// object with that name and deleting nothing.
const InitialDiscoveryName = "initial-discovery"

// How long the create waits for the operator's own webhook to begin serving, and
// how often it asks.
//
// The budget covers a cold start rather than an outage: the webhook server is
// listening within seconds of the manager starting, and the operator restarts
// once more when its cert rotator refreshes the serving material, so the window
// this crosses is a cold start plus one restart. Past the budget the run is not
// raised, which is the behavior an administrator already has a remedy for.
const (
	createRetryInterval = 500 * time.Millisecond
	createRetryDeadline = 2 * time.Minute
)

// InitialDiscovery raises one Discover run on an install that has nothing.
//
// It is a Runnable rather than a reconciler because it is not reconciling
// anything: there is no object whose desired state it converges toward, and the
// question it asks has one answer per installation.
type InitialDiscovery struct {
	client.Client

	// Namespace is where the operator runs, and where the run and its draft land.
	// A document describes one deployment, and the operator's own namespace is
	// the one place a deployment that does not exist yet can be described from.
	Namespace string
}

// NeedLeaderElection makes one replica ask the question. The create is idempotent
// without it, and the three reads are not: two replicas racing would both see an
// empty cluster and both decide to run.
func (d *InitialDiscovery) NeedLeaderElection() bool { return true }

// Start performs the check once and returns. It is not a loop: an install that
// already has something is an install that will keep having it, and an install
// that has nothing gets its run on this pass.
func (d *InitialDiscovery) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("initial-discovery")

	reason, err := d.declineReason(ctx)
	if err != nil {
		// A read that failed is not evidence of an empty cluster. Declining is the
		// conservative answer: the cost of not running is an administrator writing
		// a document by hand, and the cost of running against a fleet that is
		// already deployed is a Job on every node of it.
		log.Error(err, "the initial discovery run is declined; the cluster could not be read")
		return nil
	}
	if reason != "" {
		log.V(1).Info("the initial discovery run is not needed", "reason", reason)
		return nil
	}

	run := &simplyblockv1alpha2.OperatorOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:      InitialDiscoveryName,
			Namespace: d.Namespace,
		},
		Spec: simplyblockv1alpha2.OperatorOpsSpec{
			Action: simplyblockv1alpha2.OperatorOpsActionDiscover,
			// The run states no selector. What it produces is a draft of
			// everything the fleet has, which is what a reviewer narrows: a
			// guess at which disks somebody meant would be a guess they then
			// have to find and undo (§8.1).
			//
			// The one thing it does state is that a partition table is not by
			// itself a reason to leave a disk out. A machine that has held data
			// before carries one on every disk, so refusing them makes the run
			// meant to show a fleet what it has report that it has nothing. The
			// waiver stays narrow on its own terms: it admits a disk whose only
			// refusal is the table, and a disk that is also mounted, held by
			// the kernel, or carrying swap is refused for those instead — which
			// is what keeps a boot disk out of a draft nobody reads closely.
			Discover: &simplyblockv1alpha2.DiscoverSpec{
				DeviceFilter: &simplyblockv1alpha2.DeviceFilter{
					EnablePartitionedDevices: ptr.To(true),
				},
			},
		},
	}
	if err := d.createRun(ctx, run); err != nil {
		// Failing here would crash the manager over a convenience. The operator
		// works without the run; what is lost is the draft an administrator would
		// otherwise have found waiting.
		log.Error(err, "the initial discovery run could not be created")
		return nil
	}

	log.Info("raised the initial discovery run; its draft is what to review",
		"operatorOps", InitialDiscoveryName, "namespace", d.Namespace)
	return nil
}

// createRun writes the run, waiting out a webhook that is not serving yet.
//
// This is the one write in the operator whose admission depends on the operator:
// an OperatorOps is validated by voperatorops.simplyblock.io, which this same
// process serves, so the create races the webhook server the manager is still
// starting and the API server answers `failed calling webhook ... connection
// refused` until it is listening. Start is not a loop, so a single attempt lost
// the run for the lifetime of the installation rather than for a few seconds.
//
// Only an unreachable webhook is waited out. A webhook that answered and refused
// the spec has given an answer, and repeating the same write until the deadline
// would not change it.
func (d *InitialDiscovery) createRun(ctx context.Context, run *simplyblockv1alpha2.OperatorOps) error {
	lastErr := error(nil)
	err := wait.PollUntilContextTimeout(ctx, createRetryInterval, createRetryDeadline, true,
		func(ctx context.Context) (bool, error) {
			switch err := d.Create(ctx, run); {
			case err == nil, apierrors.IsAlreadyExists(err):
				return true, nil
			case webhookUnreachable(err):
				lastErr = err
				return false, nil
			default:
				return false, err
			}
		})
	if wait.Interrupted(err) && lastErr != nil {
		// The deadline says how long it waited, and the refusal says what it
		// waited for. The second is the one worth reading in a log.
		return lastErr
	}
	return err
}

// webhookUnreachable reports whether the API server refused the write because it
// could not call an admission webhook, rather than because a webhook rejected
// what was written.
//
// An unreachable webhook surfaces as an Internal error naming it, and a refusal
// surfaces as Forbidden or Invalid, which this deliberately does not cover.
func webhookUnreachable(err error) bool {
	return apierrors.IsInternalError(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTimeout(err)
}

// declineReason answers whether anything already exists, and says which thing. An
// empty string means the install has nothing and the run is worth raising.
func (d *InitialDiscovery) declineReason(ctx context.Context) (string, error) {
	var runs simplyblockv1alpha2.OperatorOpsList
	if err := d.List(ctx, &runs); err != nil {
		return "", fmt.Errorf("listing operator operations: %w", err)
	}
	if len(runs.Items) > 0 {
		return fmt.Sprintf("%d operator operation(s) have already run", len(runs.Items)), nil
	}

	var configs simplyblockv1alpha2.ClusterDeploymentConfigList
	if err := d.List(ctx, &configs); err != nil {
		return "", fmt.Errorf("listing deployment configs: %w", err)
	}
	if len(configs.Items) > 0 {
		return fmt.Sprintf("%d deployment config(s) already exist", len(configs.Items)), nil
	}

	var clusters simplyblockv1alpha2.StorageClusterList
	if err := d.List(ctx, &clusters); err != nil {
		return "", fmt.Errorf("listing storage clusters: %w", err)
	}
	if len(clusters.Items) > 0 {
		return fmt.Sprintf("%d storage cluster(s) are already deployed", len(clusters.Items)), nil
	}

	// The run this would raise states no filter and no selector, so it asks about
	// every machine, and UsableWorker is the same predicate it would then apply.
	// Reading it here rather than restating the conditions is what keeps the two
	// from drifting into a bootstrap that raises runs the run itself declines.
	var nodes corev1.NodeList
	if err := d.List(ctx, &nodes); err != nil {
		return "", fmt.Errorf("listing nodes: %w", err)
	}
	usable := 0
	for _, node := range nodes.Items {
		// The opt-in is a decision an administrator makes on a run they wrote.
		// This one writes no spec, so it asks the question the run it would raise
		// asks: false, and a control-plane node does not count.
		if UsableWorker(node, false) {
			usable++
		}
	}
	if usable == 0 {
		return fmt.Sprintf("none of the %d node(s) hold storage without being asked to",
			len(nodes.Items)), nil
	}

	return "", nil
}

// SetupWithManager adds the check to the manager.
func (d *InitialDiscovery) SetupWithManager(mgr ctrl.Manager) error {
	return mgr.Add(d)
}
