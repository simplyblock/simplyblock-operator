// What a document's erasure coding is answered against, and the counting the
// answer needs.
//
// The scheme decides how many storage nodes the deployment must have: ndcs+npcs
// of them to place a stripe across, plus one spare per tolerated failure for the
// rebuild to land on. Nothing below this operator checks it. The control plane
// validates the scheme itself on the cluster create and counts devices at
// activation, never nodes, so a four-worker fleet configured 4+2 activates, and
// what it has bought is a cluster that loses data on the second failure it was
// configured to survive.
//
// Both callers of this file are here for the same reason the rest of the
// document's validation has two: a draft is told on every reconcile so it can be
// edited, and the approving edit is refused because §3.2 makes an approved
// document immutable. StripeChecks is therefore exported and takes a reader
// rather than a reconciler, and the two callers differ only in what they wrap the
// result in.
//
// The counting is by slot rather than by worker because the expansion creates
// nodes by slot: a growth document naming a worker the cluster already has a node
// on produces nothing there, and counting that worker again would report a
// minimum as met by a node nobody is going to create.

package deployment

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/erasurecoding"
)

// StripeCheck is one thing wrong with what a document says about erasure
// coding, carrying the reason it is counted and reported under as well as the
// sentence somebody reads.
type StripeCheck struct {
	Reason  string
	Message string
}

// footprint is a set of storage nodes, identified the way the expansion
// identifies them, and the workers they sit on. Both numbers are needed: the
// minimum is a node count, and a node count reached by running several nodes on
// one worker has not bought the independent spare the count was asking for.
type footprint struct {
	slots   map[slotKey]struct{}
	workers map[string]struct{}
}

func newFootprint() footprint {
	return footprint{slots: map[slotKey]struct{}{}, workers: map[string]struct{}{}}
}

func (f footprint) add(worker string, slot int32) {
	f.slots[slotKey{worker: worker, slot: slot}] = struct{}{}
	f.workers[worker] = struct{}{}
}

func (f footprint) merge(other footprint) {
	for key := range other.slots {
		f.slots[key] = struct{}{}
	}
	for worker := range other.workers {
		f.workers[worker] = struct{}{}
	}
}

// nodes is how many storage nodes the deployment ends up with.
func (f footprint) nodes() int { return len(f.slots) }

// hosts is how many workers those nodes sit on.
func (f footprint) hosts() int { return len(f.workers) }

// StripeChecks answers a document's erasure coding against the deployment it
// describes.
//
// It returns at most one check, unlike the validation it feeds, because the three
// things that can be wrong here are one thing each: a scheme the control plane
// refuses has no minimum to be short of, and a deployment short of the node count
// is not also separately short of the worker count. Reporting two of them would
// put two findings on one fact.
//
// A document that names no cluster at all, or one that grows a cluster which is
// not there, is answered by the checks that own those refusals; this one has
// nothing to say about a deployment whose shape is not yet known and says
// nothing.
func StripeChecks(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]StripeCheck, error) {
	scheme, subject, slots, total, known, err := stripeSubject(ctx, reader, namespace, config)
	if err != nil || !known {
		return nil, err
	}
	total.merge(plannedFootprint(config, slots))

	if !scheme.IsSupported() {
		// The minimum of a scheme that does not exist is not a fact about
		// anything, so it is not reported beside it.
		return []StripeCheck{{
			Reason: StripeUnsupported,
			Message: fmt.Sprintf(
				"%s is %s, which is not a scheme simplyblock supports (%s); the control "+
					"plane refuses the cluster it would be created with, and that refusal "+
					"lands after approval has made this document immutable",
				subject, scheme, erasurecoding.SupportedNotation()),
		}}, nil
	}

	minimum := scheme.MinimumNodes()
	if total.nodes() < minimum {
		return []StripeCheck{{
			Reason: StripeBelowMinimumNodes,
			Message: fmt.Sprintf("%s is %s, which needs at least %d %s%s, and this document %s %d",
				subject, scheme, minimum, plural(minimum, "storage node", "storage nodes"),
				placementPhrase(scheme), leavesPhrase(config), total.nodes()),
		}}, nil
	}

	if total.hosts() < minimum {
		return []StripeCheck{{
			Reason: StripeBelowMinimumWorkers,
			Message: fmt.Sprintf(
				"%s is %s, which needs at least %d storage nodes, and the %d this "+
					"deployment has sit on %d %s; every node of a worker fails with the "+
					"worker, so the spare the scheme rebuilds onto is not a spare",
				subject, scheme, minimum, total.nodes(), total.hosts(),
				plural(total.hosts(), "worker", "workers")),
		}}, nil
	}
	return nil, nil
}

// placementPhrase accounts for the minimum, because a number a reviewer cannot
// account for is a number they cannot act on: it is the stripe's own width plus
// a spare for each failure the stripe survives.
func placementPhrase(scheme erasurecoding.Scheme) string {
	if scheme.ParityChunks == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d to place a stripe across and %d %s to rebuild onto)",
		scheme.DataChunks+scheme.ParityChunks, scheme.ParityChunks,
		plural(scheme.ParityChunks, "spare", "spares"))
}

// leavesPhrase says whether the count that follows is what the document builds
// or what it leaves behind, which are different things for a growth document.
func leavesPhrase(config *simplyblockv1alpha2.ClusterDeploymentConfig) string {
	if config.Spec.ClusterRef != "" {
		return "leaves the cluster with"
	}
	return "produces"
}

// stripeSubject resolves what the document's erasure coding is, what to call it
// in a message, how many nodes each of its workers runs, and what the cluster
// already has. known is false for a document whose cluster cannot be resolved,
// which is a refusal of its own elsewhere.
func stripeSubject(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (scheme erasurecoding.Scheme, subject string, slots int32, existing footprint, known bool, err error) {
	if config.Spec.ClusterRef == "" {
		template := config.Spec.Cluster
		if template == nil {
			return scheme, "", 0, existing, false, nil
		}
		return erasurecoding.SchemeOf(template.Stripe), "spec.cluster.stripe",
			slotsOf(template.SocketsToUse, template.NodesPerSocket), newFootprint(), true, nil
	}

	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: namespace, Name: config.Spec.ClusterRef}
	switch err := reader.Get(ctx, key, &cluster); {
	case apierrors.IsNotFound(err):
		return scheme, "", 0, existing, false, nil
	case err != nil:
		return scheme, "", 0, existing, false,
			fmt.Errorf("reading StorageCluster %s: %w", config.Spec.ClusterRef, err)
	}

	held, err := existingFootprint(ctx, reader, namespace, cluster.Name)
	if err != nil {
		return scheme, "", 0, existing, false, err
	}
	return erasurecoding.SchemeOf(cluster.Spec.Stripe),
		fmt.Sprintf("the stripe of cluster %s", cluster.Name),
		slotsPerWorker(&cluster), held, true, nil
}

// plannedFootprint is every storage node the document would produce, one per
// slot per worker.
func plannedFootprint(
	config *simplyblockv1alpha2.ClusterDeploymentConfig, slots int32,
) footprint {
	planned := newFootprint()
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				for slot := int32(0); slot < slots; slot++ {
					planned.add(worker, slot)
				}
			}
		}
	}
	return planned
}

// existingFootprint is every storage node a cluster already has.
func existingFootprint(
	ctx context.Context, reader client.Reader, namespace, cluster string,
) (footprint, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := reader.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return footprint{}, fmt.Errorf("listing the nodes of cluster %s: %w", cluster, err)
	}
	held := newFootprint()
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != cluster {
			continue
		}
		slot := int32(0)
		if node.Spec.Slot != nil {
			slot = *node.Spec.Slot
		}
		held.add(node.Spec.WorkerNode, slot)
	}
	return held, nil
}
