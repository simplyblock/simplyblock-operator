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
// previous result exists, and it is three questions rather than one:
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
//
// Any one of the three is enough to decline. Together, they mean the run happens
// on the install that has nothing, and never again.
//
// It runs under leader election, so one replica asks. The create is idempotent by
// name as well, because two replicas answering the same three questions at once
// would both conclude yes.

package deployment

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// InitialDiscoveryName is what the run is called. It is fixed rather than
// generated so that the create is idempotent by name on top of the guard below,
// and so that an administrator who does not want it can say so by writing an
// object with that name and deleting nothing.
const InitialDiscoveryName = "initial-discovery"

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
			// The run states no filter and no selector. What it produces is a
			// draft of everything the fleet has, which is what a reviewer
			// narrows: a guess at which disks somebody meant would be a guess
			// they then have to find and undo (§8.1).
			Discover: &simplyblockv1alpha2.DiscoverSpec{},
		},
	}
	if err := d.Create(ctx, run); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
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

	return "", nil
}

// SetupWithManager adds the check to the manager.
func (d *InitialDiscovery) SetupWithManager(mgr ctrl.Manager) error {
	return mgr.Add(d)
}
