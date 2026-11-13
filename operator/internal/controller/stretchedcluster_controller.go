// The StretchedCluster controller: the hub carries one stretched cluster's
// request to its two sites, one site-deployment flow each, and joins them.

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	finalizerStretchedCluster = "storage.simplyblock.io/stretchedcluster-release"
	// stretchedClusterIDLabel marks the works and views of one request, by its
	// UID, so its deletion finds every one of them.
	stretchedClusterIDLabel = "storage.simplyblock.io/stretched-cluster"

	ConditionStretchedDelivered = "Delivered"
	ConditionStretchedDrafted   = "Drafted"
	ConditionStretchedApproved  = "Approved"
	ConditionStretchedReady     = "Ready"
)

// managedClusterGVK is OCM's ManagedCluster, read for whether the hub reaches a
// site's API server. It is read unstructured, as the views are.
var managedClusterGVK = schema.GroupVersionKind{
	Group: "cluster.open-cluster-management.io", Version: "v1", Kind: "ManagedCluster",
}

// StretchedControlPlane is the hub's own control plane, read for the cluster
// secret the follower site adopts the cluster with.
type StretchedControlPlane interface {
	GetCluster(ctx context.Context, clusterUUID string) (webapi.ClusterResponse, error)
}

// StretchedClusterReconciler reconciles a StretchedCluster.
type StretchedClusterReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Recorder     events.EventRecorder
	ControlPlane StretchedControlPlane
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=stretchedclusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=stretchedclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=stretchedclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=work.open-cluster-management.io,resources=manifestworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=view.open-cluster-management.io,resources=managedclusterviews,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=managedclusters,verbs=get;list;watch

// SetupWithManager registers the controller. The works and the views live in
// the sites' namespaces on the hub, where an owner reference to the request
// cannot point, so the sites' answers are read on the requeue cadence.
func (r *StretchedClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StretchedCluster{}).
		Named("stretchedcluster").
		Complete(r)
}

// Reconcile runs the leader's site flow, then the follower's, which waits on the
// leader's cluster for its approval and its credentials, and joins the two into
// the request's phase.
func (r *StretchedClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sc simplyblockv1alpha2.StretchedCluster
	if err := r.Get(ctx, req.NamespacedName, &sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !sc.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, &sc)
	}
	if !controllerutil.ContainsFinalizer(&sc, finalizerStretchedCluster) {
		controllerutil.AddFinalizer(&sc, finalizerStretchedCluster)
		return ctrl.Result{Requeue: true}, r.Update(ctx, &sc)
	}

	leader, follower, ok := stretchedRoles(&sc)
	if !ok {
		// The schema holds exactly one leader; a request that has none was
		// written past it and cannot be carried.
		return r.setStretchedPhase(ctx, &sc, simplyblockv1alpha2.StretchedClusterPhaseFailed,
			"the request does not name exactly one Leader and one Follower", nil)
	}

	leaderReading, err := r.runSite(ctx, &sc, leader, follower, "", "")
	if err != nil {
		return ctrl.Result{}, err
	}
	var uuid, secret string
	if leaderReading.StorageCluster != nil {
		uuid = leaderReading.StorageCluster.UUID
	}
	// The leader's backend cluster keeps its UUID for life, so the one recorded
	// stands when this pass could not read it: a failed leader draft is read no
	// further than the draft, and a lost read must not take the follower's
	// approval and credentials with it.
	if uuid == "" {
		uuid = sc.Status.ClusterUUID
	}
	if uuid != "" && r.ControlPlane != nil {
		found, err := r.ControlPlane.GetCluster(ctx, uuid)
		if err != nil {
			logf.FromContext(ctx).Error(err, "the leader's cluster could not be read for the follower's credentials",
				"cluster", uuid)
		} else {
			secret = found.Secret
		}
	}
	followerReading, err := r.runSite(ctx, &sc, follower, leader, uuid, secret)
	if err != nil {
		return ctrl.Result{}, err
	}

	readings := []siteReading{leaderReading, followerReading}
	sites := []simplyblockv1alpha2.StretchedSite{leader, follower}
	reachable := r.reachability(ctx, sites)
	phase, message := stretchedPhase(sites, readings, reachable)
	return r.setStretchedPhase(ctx, &sc, phase, message, func(s *simplyblockv1alpha2.StretchedClusterStatus) {
		s.ClusterUUID = uuid
		// Both sites read the one control-plane record; a lost site cannot report
		// its own, so the survivor's reading is the one that names it.
		s.LostSite = ""
		for _, reading := range readings {
			if reading.LostSite != "" {
				s.LostSite = reading.LostSite
				break
			}
		}
		s.Sites = nil
		for i, site := range sites {
			reading := readings[i]
			s.Sites = append(s.Sites, simplyblockv1alpha2.StretchedClusterSiteStatus{
				Cluster: site.Cluster, Site: site.Site, Role: site.Role,
				Phase: reading.Phase, Message: reading.Message, WorkName: reading.WorkName,
				Reachable: reachable[i], SiteRecord: reading.SiteRecord,
				Draft: reading.Draft, StorageCluster: reading.StorageCluster,
			})
		}
		both := func(check func(siteReading) bool) bool { return check(readings[0]) && check(readings[1]) }
		setStretchedCondition(s, ConditionStretchedDelivered, both(func(r siteReading) bool { return r.Delivered }),
			"both works are applied on their sites")
		setStretchedCondition(s, ConditionStretchedDrafted, both(func(r siteReading) bool { return draftHasNodes(r.Draft) }),
			"both drafts name nodes")
		setStretchedCondition(s, ConditionStretchedApproved, both(func(r siteReading) bool { return r.Draft != nil && r.Draft.Approved }),
			"both sites' drafts are approved")
		setStretchedCondition(s, ConditionStretchedReady, phase == simplyblockv1alpha2.StretchedClusterPhaseReady,
			"both StorageClusters are Online on one backend cluster")
	})
}

// stretchedRoles splits the two sites into the leader and the follower.
func stretchedRoles(sc *simplyblockv1alpha2.StretchedCluster) (leader, follower simplyblockv1alpha2.StretchedSite, ok bool) {
	var leaders, followers []simplyblockv1alpha2.StretchedSite
	for _, site := range sc.Spec.Sites {
		switch site.Role {
		case simplyblockv1alpha2.StretchedRoleLeader:
			leaders = append(leaders, site)
		case simplyblockv1alpha2.StretchedRoleFollower:
			followers = append(followers, site)
		}
	}
	if len(leaders) != 1 || len(followers) != 1 {
		return leader, follower, false
	}
	return leaders[0], followers[0], true
}

// runSite carries the request to one site and reads it back. The leader's draft
// is approved as soon as the request is; the follower's only once the leader's
// cluster exists (its UUID), together with the credentials to adopt it (secret),
// which the work delivers as the upgrade Secret the site's StorageCluster
// controller adopts through.
func (r *StretchedClusterReconciler) runSite(
	ctx context.Context, sc *simplyblockv1alpha2.StretchedCluster,
	site, peer simplyblockv1alpha2.StretchedSite, uuid, secret string,
) (siteReading, error) {
	siteNS := orDefault(site.SiteNamespace, "simplyblock")
	approved := sc.Spec.Approved
	if site.Role == simplyblockv1alpha2.StretchedRoleFollower {
		approved = approved && uuid != ""
	}
	// Approval is one-way on the site, which refuses an apply that withdraws it
	// and then applies nothing else in the work.
	if previous := previousDraft(sc, site.Cluster); previous != nil && previous.Approved {
		approved = true
	}
	template := sizingTemplate(sc.Spec.Sizing)
	template["name"] = sc.Spec.ClusterName
	template["enableSyncReplication"] = true
	template["stretched"] = map[string]any{
		"site": site.Site, "peerSite": peer.Site, "peerCluster": peer.Cluster, "role": string(site.Role),
	}

	flow := siteFlow{
		ManagedCluster: site.Cluster,
		SiteNamespace:  siteNS,
		DraftName:      stretchedDraftName(sc),
		Discover:       site.Discover,
		Labels:         map[string]string{stretchedClusterIDLabel: string(sc.UID)},
		WorkName:       stretchedWorkName(sc, site.Site),
		DraftSpec:      draftSpec(approved, template, sc.Spec.Images),
		PreviousDraft:  previousDraft(sc, site.Cluster),
	}
	if site.Role == simplyblockv1alpha2.StretchedRoleFollower && uuid != "" && secret != "" {
		flow.Extra = append(flow.Extra, map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name": fmt.Sprintf("simplyblock-%s-upgrade", sc.Spec.ClusterName), "namespace": siteNS,
				"labels": flow.Labels,
			},
			"type":       string(corev1.SecretTypeOpaque),
			"stringData": map[string]any{"uuid": uuid, "secret": secret},
		})
	}

	work, err := flow.ensureWork(ctx, r.Client)
	if err != nil {
		return siteReading{}, fmt.Errorf("carrying the request to %s: %w", site.Cluster, err)
	}
	return flow.read(ctx, r.Client, work)
}

// previousDraft is the site's draft as the status last recorded it, which gates
// the apply onto it.
func previousDraft(sc *simplyblockv1alpha2.StretchedCluster, cluster string) *simplyblockv1alpha2.StorageSiteDraft {
	for _, s := range sc.Status.Sites {
		if s.Cluster == cluster {
			return s.Draft
		}
	}
	return nil
}

// reachability reads whether the hub reaches each site's API server, nil where
// that cannot be read (no ManagedCluster, or OCM's kind not served).
func (r *StretchedClusterReconciler) reachability(
	ctx context.Context, sites []simplyblockv1alpha2.StretchedSite,
) []*bool {
	out := make([]*bool, len(sites))
	for i, site := range sites {
		mc := &unstructured.Unstructured{}
		mc.SetGroupVersionKind(managedClusterGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: site.Cluster}, mc); err != nil {
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(mc.Object, "status", "conditions")
		for _, raw := range conditions {
			c, _ := raw.(map[string]any)
			if c["type"] == "ManagedClusterConditionAvailable" {
				available := c["status"] == string(metav1.ConditionTrue)
				out[i] = &available
			}
		}
	}
	return out
}

// stretchedPhase joins the two sites. A failed site fails the request; a site
// still discovering keeps it Discovering; two unapproved drafts are Drafted;
// then the leader's cluster is created, the follower joins, and two Online
// clusters are Ready, or Degraded while the control plane records a site out
// or the hub cannot reach one.
func stretchedPhase(
	sites []simplyblockv1alpha2.StretchedSite, readings []siteReading, reachable []*bool,
) (simplyblockv1alpha2.StretchedClusterPhase, string) {
	const (
		failed      = simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed
		discovering = simplyblockv1alpha2.StorageSiteDeploymentPhaseDiscovering
		drafted     = simplyblockv1alpha2.StorageSiteDeploymentPhaseDrafted
		online      = simplyblockv1alpha2.StorageSiteDeploymentPhaseOnline
	)
	leader, follower := readings[0], readings[1]
	for i, reading := range readings {
		if reading.Phase == failed {
			return simplyblockv1alpha2.StretchedClusterPhaseFailed,
				fmt.Sprintf("site %s (%s): %s", sites[i].Site, sites[i].Cluster, reading.Message)
		}
	}
	for i, reading := range readings {
		if reading.Phase == discovering || reading.Phase == "" {
			return simplyblockv1alpha2.StretchedClusterPhaseDiscovering,
				fmt.Sprintf("site %s (%s): %s", sites[i].Site, sites[i].Cluster, reading.Message)
		}
	}
	if leader.Phase == drafted {
		return simplyblockv1alpha2.StretchedClusterPhaseDrafted, "both drafts are written and await approval"
	}
	if leader.Phase != online {
		return simplyblockv1alpha2.StretchedClusterPhaseLeaderCreating,
			fmt.Sprintf("leader site %s: %s", sites[0].Site, leader.Message)
	}
	if follower.Phase != online {
		return simplyblockv1alpha2.StretchedClusterPhaseFollowerJoining,
			fmt.Sprintf("follower site %s: %s", sites[1].Site, follower.Message)
	}
	for i, reading := range readings {
		if reading.SiteRecord != "" {
			return simplyblockv1alpha2.StretchedClusterPhaseDegraded,
				fmt.Sprintf("the control plane records a site as %s, read on site %s", reading.SiteRecord, sites[i].Site)
		}
		if reachable[i] != nil && !*reachable[i] {
			return simplyblockv1alpha2.StretchedClusterPhaseDegraded,
				fmt.Sprintf("the hub does not reach managed cluster %s", sites[i].Cluster)
		}
	}
	return simplyblockv1alpha2.StretchedClusterPhaseReady, "both sites are Online on one backend cluster"
}

// setStretchedPhase writes the phase, the message and the mutation, with the
// generation they were computed from, and records a phase change as an event.
func (r *StretchedClusterReconciler) setStretchedPhase(
	ctx context.Context, sc *simplyblockv1alpha2.StretchedCluster,
	phase simplyblockv1alpha2.StretchedClusterPhase, message string,
	mutate func(*simplyblockv1alpha2.StretchedClusterStatus),
) (ctrl.Result, error) {
	previous := sc.Status.Phase
	base := client.MergeFrom(sc.DeepCopy())
	if mutate != nil {
		mutate(&sc.Status)
	}
	sc.Status.Phase = phase
	sc.Status.Message = message
	sc.Status.ObservedGeneration = sc.Generation
	if err := r.Status().Patch(ctx, sc, base); err != nil {
		return ctrl.Result{}, err
	}
	if previous != phase && r.Recorder != nil {
		kind := corev1.EventTypeNormal
		if phase == simplyblockv1alpha2.StretchedClusterPhaseFailed || phase == simplyblockv1alpha2.StretchedClusterPhaseDegraded {
			kind = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(sc, nil, kind, string(phase), string(phase), "%s", message)
	}
	switch phase {
	case simplyblockv1alpha2.StretchedClusterPhaseReady, simplyblockv1alpha2.StretchedClusterPhaseFailed:
		return ctrl.Result{RequeueAfter: storageSiteDeploymentOnlineRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: storageSiteDeploymentRequeue}, nil
}

func setStretchedCondition(s *simplyblockv1alpha2.StretchedClusterStatus, kind string, ok bool, message string) {
	status, reason := metav1.ConditionFalse, "NotYet"
	if ok {
		status, reason = metav1.ConditionTrue, "Met"
	}
	meta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: s.ObservedGeneration,
	})
}

// reconcileDeletion releases both sites' works and views and removes the
// finalizer. The works orphan their resources, so nothing on the sites goes.
func (r *StretchedClusterReconciler) reconcileDeletion(
	ctx context.Context, sc *simplyblockv1alpha2.StretchedCluster,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sc, finalizerStretchedCluster) {
		return ctrl.Result{}, nil
	}
	for _, site := range sc.Spec.Sites {
		flow := siteFlow{
			ManagedCluster: site.Cluster,
			Labels:         map[string]string{stretchedClusterIDLabel: string(sc.UID)},
			WorkName:       stretchedWorkName(sc, site.Site),
		}
		done, err := flow.release(ctx, r.Client)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}
	controllerutil.RemoveFinalizer(sc, finalizerStretchedCluster)
	return ctrl.Result{}, r.Update(ctx, sc)
}

// stretchedHash is a short, deterministic id of the request for the names of
// its works and views, which must be bounded and found again on a restart.
func stretchedHash(sc *simplyblockv1alpha2.StretchedCluster) string {
	h := sha256.Sum256([]byte(sc.Namespace + "/" + sc.Name))
	return fmt.Sprintf("%x", h[:6])
}

// stretchedWorkName is the work carrying the request to one site, in that
// site's namespace on the hub.
func stretchedWorkName(sc *simplyblockv1alpha2.StretchedCluster, site string) string {
	s := strings.ToLower(strings.NewReplacer("_", "-", ".", "-").Replace(site))
	if len(s) > 20 {
		s = s[:20]
	}
	return "sbstr-" + stretchedHash(sc) + "-" + s
}

// stretchedViewName is one of a site's views.
func stretchedViewName(sc *simplyblockv1alpha2.StretchedCluster, site, suffix string) string {
	return stretchedWorkName(sc, site) + "-" + suffix
}

// draftSpec is what the hub applies onto a site's draft: the approval, the
// cluster template, and the images both sites run, when the request pins any.
func draftSpec(approved bool, template map[string]any, images *simplyblockv1alpha2.DeploymentImages) map[string]any {
	spec := map[string]any{"approved": approved, "cluster": template}
	if images == nil {
		return spec
	}
	slots := map[string]any{}
	for name, slot := range map[string]*simplyblockv1alpha2.ImageSpec{
		"nodeAgent": images.NodeAgent, "spdk": images.SPDK, "spdkProxy": images.SPDKProxy,
	} {
		if slot == nil {
			continue
		}
		image := map[string]any{"image": slot.Image}
		if slot.ImagePullPolicy != "" {
			image["imagePullPolicy"] = string(slot.ImagePullPolicy)
		}
		slots[name] = image
	}
	if len(slots) > 0 {
		spec["images"] = slots
	}
	return spec
}

// stretchedDraftName is the draft the discovery writes on each site.
func stretchedDraftName(sc *simplyblockv1alpha2.StretchedCluster) string {
	name := sc.Spec.ClusterName + "-draft"
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}
