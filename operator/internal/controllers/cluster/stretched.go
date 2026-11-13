// The StorageCluster controller's part in a stretched cluster: one backend
// cluster whose two sites are two Kubernetes clusters, each running this
// operator as a standalone installation. The leader site creates the backend
// cluster on the ordinary creation path. The follower site never creates it and
// never deletes it: it waits in CheckingControlPlane until the hub delivers the
// credentials to adopt the cluster its leader created, and lets go of it on
// deletion. Both sites project the control plane's site record into
// status.stretched (design-stretched-sync-replication-ramen.md §5, §6.1).

package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

func upgradeSecretName(cluster *simplyblockv1alpha2.StorageCluster) string {
	return fmt.Sprintf("simplyblock-%s-upgrade", cluster.Name)
}

// awaitLeader is a follower's CheckingControlPlane. The follower adopts only
// through the credentials the hub delivers as the upgrade Secret, because an
// adoption by name yields no cluster secret, and it waits for them for as long
// as its leader takes: the step's deadline bounds a step doing work, not one
// waiting on another site, so this runs before the deadline is checked.
func (r *StorageClusterReconciler) awaitLeader(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	machine *statemachine.Machine[simplyblockv1alpha2.StorageClusterStep],
) (ctrl.Result, error) {
	if err := r.API.Ready(ctx); err != nil {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
			FDBNotReady, FDBNotReady,
			"The control plane is not ready to accept a cluster: %v", err)
		return ctrl.Result{RequeueAfter: clusterRetry}, r.note(ctx, cluster,
			"waiting for the control plane to become ready")
	}
	if claim, found, err := r.upgradeClaim(ctx, cluster); err != nil {
		return ctrl.Result{RequeueAfter: clusterRetry}, err
	} else if found {
		// The preflight (§5.4) runs here rather than in Adopting: what it finds
		// is a cluster that cannot be adopted yet, and a hold in this step has
		// no deadline to run out.
		reason, refusal, err := r.followerPreflight(ctx, cluster, claim)
		if err != nil {
			return ctrl.Result{RequeueAfter: clusterRetry}, err
		}
		if refusal != "" {
			return ctrl.Result{RequeueAfter: clusterRetry}, r.noteOnce(ctx, cluster,
				corev1.EventTypeWarning, reason, refusal)
		}
		return r.enterCreationStep(ctx, cluster, machine,
			simplyblockv1alpha2.StorageClusterStepAdopting)
	}

	message := fmt.Sprintf(
		"follower site: waiting for the leader site to create backend cluster %s", cluster.Name)
	if _, listed, err := r.API.ClusterByName(ctx, cluster.Name); err == nil && listed {
		message = fmt.Sprintf(
			"follower site: backend cluster %s exists; waiting for the hub to deliver "+
				"the credentials to adopt it in Secret %s", cluster.Name, upgradeSecretName(cluster))
	}
	return ctrl.Result{RequeueAfter: clusterRetry}, r.noteOnce(ctx, cluster,
		corev1.EventTypeNormal, PeerClusterMissing, message)
}

// followerPreflight checks the backend cluster the hub handed a follower before
// the follower adopts it (§5.4), and reports the event reason and the refusal, or
// "" when the adoption may go ahead. The cluster must be a sync-replication
// cluster, and no node on the follower's own site may be another installation's:
// one whose UUID none of this site's StorageNodes carries.
func (r *StorageClusterReconciler) followerPreflight(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, claim adoption,
) (string, string, error) {
	if refusal := refuseFollowerAdoption(cluster, claim); refusal != "" {
		return SyncReplicationMismatch, refusal, nil
	}
	if site := cluster.Spec.Stretched.Site; claim.LeaderSite == site {
		return RoleConflict, fmt.Sprintf(
			"follower site: backend cluster %s names this site, %s, as its leader site; adopting it as "+
				"follower would make two leaders, so the adoption is held until the record names the "+
				"other site", claim.UUID, site), nil
	}

	nodes, err := r.API.StorageNodes(webapi.WithBearerToken(ctx, claim.Secret), claim.UUID)
	if err != nil {
		return "", "", fmt.Errorf("listing the nodes of backend cluster %s: %w", claim.UUID, err)
	}
	var local simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &local, client.InNamespace(cluster.Namespace)); err != nil {
		return "", "", err
	}
	own := map[string]struct{}{}
	for i := range local.Items {
		if node := &local.Items[i]; !node.IsRemote() && node.Status.UUID != "" {
			own[node.Status.UUID] = struct{}{}
		}
	}
	var foreign []string
	for _, node := range nodes {
		if node.Site != cluster.Spec.Stretched.Site {
			continue
		}
		if _, ours := own[node.UUID]; !ours {
			foreign = append(foreign, node.UUID)
		}
	}
	if len(foreign) > 0 {
		return ForeignNodeOnSite, fmt.Sprintf(
			"follower site: backend cluster %s already has node(s) %s on site %s that no StorageNode "+
				"of this site carries, so another installation holds the site; the adoption is held",
			claim.UUID, strings.Join(foreign, ", "), cluster.Spec.Stretched.Site), nil
	}
	return r.workersReachable(ctx, cluster, claim, local.Items)
}

// workersReachable has the control plane probe the management addresses of the
// workers this site's StorageNodes run on, which it must reach once they are
// added as nodes (§5.4, P0-3). A control plane without the probe is not a
// refusal: the node add reports an unreachable worker later.
func (r *StorageClusterReconciler) workersReachable(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, claim adoption,
	nodes []simplyblockv1alpha2.StorageNode,
) (string, string, error) {
	workerOf := map[string]string{}
	var addresses []string
	for i := range nodes {
		node := &nodes[i]
		if node.IsRemote() || node.Spec.ClusterRef != cluster.Name || node.Spec.WorkerNode == "" {
			continue
		}
		var worker corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: node.Spec.WorkerNode}, &worker); err != nil {
			return "", "", client.IgnoreNotFound(err)
		}
		for _, addr := range worker.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP && workerOf[addr.Address] == "" {
				workerOf[addr.Address] = worker.Name
				addresses = append(addresses, addr.Address)
				break
			}
		}
	}
	if len(addresses) == 0 {
		return "", "", nil
	}
	probed, err := r.API.Reachability(webapi.WithBearerToken(ctx, claim.Secret), claim.UUID, addresses)
	if errors.Is(err, ErrReachabilityUnsupported) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("probing this site's workers from the control plane: %w", err)
	}
	var unreachable []string
	for _, p := range probed {
		if !p.Reachable {
			unreachable = append(unreachable, fmt.Sprintf("%s (%s)", workerOf[p.Address], p.Address))
		}
	}
	if len(unreachable) > 0 {
		return WorkerUnreachable, fmt.Sprintf(
			"follower site: the control plane of backend cluster %s does not reach worker(s) %s of site %s "+
				"on the management network; their nodes could not be added, so the adoption is held",
			claim.UUID, strings.Join(unreachable, ", "), cluster.Spec.Stretched.Site), nil
	}
	return "", "", nil
}

// creatingLeaderSite is the leader site a create names: a stretched cluster's
// leader names its own site, any other cluster none.
func creatingLeaderSite(cluster *simplyblockv1alpha2.StorageCluster) string {
	if s := cluster.Spec.Stretched; s != nil && s.Role == simplyblockv1alpha2.StretchedRoleLeader {
		return s.Site
	}
	return ""
}

// recordLeaderSite keeps the cluster record naming the site acting as leader,
// so the record follows a role swap (§11.1): the leader writes its own site when
// the record names another or none. The control plane refuses a site it
// records as lost, and the leader does not ask for it. A failed write is
// logged and tried again on the next pass.
func (r *StorageClusterReconciler) recordLeaderSite(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, reading subscriptions.ClusterDTO,
) {
	s := cluster.Spec.Stretched
	if s == nil || s.Role != simplyblockv1alpha2.StretchedRoleLeader ||
		reading.LeaderSite == s.Site || reading.LostSite == s.Site {
		return
	}
	if err := r.API.SetLeaderSite(ctx, cluster.Status.UUID, s.Site); err != nil {
		logf.FromContext(ctx).Error(err, "the cluster record could not be set to name this site its leader",
			"cluster", cluster.Name, "site", s.Site)
		return
	}
	was := reading.LeaderSite
	if was == "" {
		was = "no site"
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, LeaderSiteRecorded, LeaderSiteRecorded,
		"the cluster record now names site %s its leader, where it named %s", s.Site, was)
}

// refuseFollowerAdoption reports why a follower cannot take over what it was
// handed, or "" when it can: a stretched cluster is a sync-replication cluster,
// so a follower handed any other cluster refuses rather than joining it.
func refuseFollowerAdoption(cluster *simplyblockv1alpha2.StorageCluster, found adoption) string {
	if !cluster.IsStretchedFollower() || found.SyncReplication {
		return ""
	}
	return fmt.Sprintf("follower site: backend cluster %s is not a sync-replication cluster, "+
		"so it cannot be one site of a stretched cluster; the adoption is refused", found.UUID)
}

// noteOnce writes message to status and raises it as an event, but only when
// it changed, so a hold that requeues every pass leaves one event rather than
// one per pass.
func (r *StorageClusterReconciler) noteOnce(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
	eventType, reason, message string,
) error {
	if cluster.Status.Message == message {
		return nil
	}
	r.Recorder.Eventf(cluster, nil, eventType, reason, reason, "%s", message)
	return r.note(ctx, cluster, message)
}

// stretchedStatus is this site's view of its stretched cluster: the role it
// acts on, the control plane's site record, and the other site's nodes as this
// site lists them. Nil for a cluster that is not one site of a stretched one.
func (r *StorageClusterReconciler) stretchedStatus(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, reading subscriptions.ClusterDTO,
) (*simplyblockv1alpha2.StretchedStatus, error) {
	if cluster.Spec.Stretched == nil {
		return nil, nil
	}
	status := &simplyblockv1alpha2.StretchedStatus{
		Role:       cluster.Spec.Stretched.Role,
		LostSite:   reading.LostSite,
		SiteRecord: reading.LostSiteState,
	}

	// The namespace holds exactly one StorageCluster (the validating webhook
	// refuses a second), so its remote nodes are this cluster's peer nodes.
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != cluster.Name || !node.IsRemote() {
			continue
		}
		status.PeerNodes++
		if node.Status.Status == utils.NodeStatusOnline {
			status.PeerNodesOnline++
		}
	}
	status.SiteLink = r.probeSiteLink(ctx, cluster)
	return status, nil
}

// siteLinkProbeTimeout bounds one check of the site link, which crosses the
// relay twice and must not hold up the status write behind it.
const siteLinkProbeTimeout = 5 * time.Second

// probeSiteLink asks the other site's operator to answer over the site link,
// addressed by its managed cluster's name, and reports whether it did. It
// reports nothing when this operator runs no site link.
func (r *StorageClusterReconciler) probeSiteLink(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) simplyblockv1alpha2.StretchedSiteLink {
	if r.SiteLink == nil {
		return ""
	}
	peer := cluster.Spec.Stretched.PeerCluster
	if peer == "" {
		peer = cluster.Spec.Stretched.PeerSite
	}
	ctx, cancel := context.WithTimeout(ctx, siteLinkProbeTimeout)
	defer cancel()
	if err := r.SiteLink.ProbePeer(ctx, peer); err != nil {
		logf.FromContext(ctx).Info("the site link does not reach the other site's operator",
			"peer", peer, "error", err.Error())
		return simplyblockv1alpha2.StretchedSiteLinkUnreachable
	}
	return simplyblockv1alpha2.StretchedSiteLinkConnected
}

// announceStretched raises the events a change of the stretched status is owed:
// the control plane recording a site as out, moving its drain or fence on, or
// clearing it; and a role swap taken up.
func (r *StorageClusterReconciler) announceStretched(
	cluster *simplyblockv1alpha2.StorageCluster, before, after *simplyblockv1alpha2.StretchedStatus,
) {
	if after == nil {
		return
	}
	var wasSite, wasRecord string
	var wasRole simplyblockv1alpha2.StretchedRole
	if before != nil {
		wasSite, wasRecord, wasRole = before.LostSite, before.SiteRecord, before.Role
	}
	if wasSite != after.LostSite || wasRecord != after.SiteRecord {
		eventType := corev1.EventTypeWarning
		message := fmt.Sprintf("the control plane records site %s as %s", after.LostSite, after.SiteRecord)
		if after.LostSite == "" {
			eventType = corev1.EventTypeNormal
			message = fmt.Sprintf("the control plane no longer records site %s as out", wasSite)
		}
		r.Recorder.Eventf(cluster, nil, eventType, SiteRecordChanged, SiteRecordChanged, "%s", message)
	}
	var wasLink simplyblockv1alpha2.StretchedSiteLink
	if before != nil {
		wasLink = before.SiteLink
	}
	if wasLink != after.SiteLink && after.SiteLink != "" {
		eventType, message := corev1.EventTypeNormal, "the site link reaches the other site's operator"
		if after.SiteLink == simplyblockv1alpha2.StretchedSiteLinkUnreachable {
			eventType, message = corev1.EventTypeWarning, "the site link does not reach the other site's operator"
		}
		r.Recorder.Eventf(cluster, nil, eventType, SiteLinkChanged, SiteLinkChanged, "%s", message)
	}
	if wasRole != "" && wasRole != after.Role {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, RoleChanged, RoleChanged,
			"this site's role changed from %s to %s", wasRole, after.Role)
	}
}

// SiteLinkProber checks the site link to another site's operator, named by its
// managed cluster.
type SiteLinkProber interface {
	ProbePeer(ctx context.Context, site string) error
}
