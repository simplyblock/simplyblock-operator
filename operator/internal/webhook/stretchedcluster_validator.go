// The StretchedCluster role-swap guard: a validating webhook on UPDATE that
// refuses to swap the two sites' roles unless both sites' StorageClusters are
// Online, or the leader's site is lost and fenced. A swap moves the cluster's
// cluster-wide operations from one site's operator to the other's. Landing it
// on a cluster still being created, or one that is degraded, would hand them
// over mid-flight. When the leader's site is gone for long, handing them to the
// survivor is the point (design-stretched-sync-replication-ramen.md §5.1, §11.1),
// and the control plane's fence is what proves the old leader cannot act.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	atlascp "github.com/simplyblock/atlas/controlplane"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-stretchedcluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=stretchedclusters,verbs=update,versions=v1alpha2,name=vstretchedcluster.simplyblock.io,admissionReviewVersions=v1

// StretchedClusterValidator refuses a role swap a site is not ready for.
//
// It holds no client: whether both sites are Online is what the request's own
// status projects, read from the object being updated.
type StretchedClusterValidator struct {
	// Backend is the control plane, read for the lost site, its fence, and the
	// nodes' status.
	Backend SwapBackend
}

// SwapBackend is the control plane as the guard reads it.
type SwapBackend interface {
	Cluster(ctx context.Context, clusterID string) (atlascp.Cluster, error)
	StorageNodes(ctx context.Context, clusterID string) ([]utils.NodeStatusResponse, error)
}

func (v *StretchedClusterValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Update {
		return admission.Allowed("")
	}
	var oldObj, newObj simplyblockv1alpha2.StretchedCluster
	if err := json.Unmarshal(req.OldObject.Raw, &oldObj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if err := json.Unmarshal(req.Object.Raw, &newObj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if !rolesSwapped(&oldObj, &newObj) {
		return admission.Allowed("")
	}
	// The control plane decides, not the request's projections: those are read
	// through the sites' views on their own schedule and lag a site going down by
	// minutes, which is the window a swap must not land in before the fence.
	if uuid := oldObj.Status.ClusterUUID; uuid != "" {
		lost, record, refusal := v.backendRefusal(ctx, &oldObj, uuid)
		if refusal != nil {
			return *refusal
		}
		if lost != "" {
			return swapWithALostSite(&oldObj, lost, record)
		}
	}
	for _, site := range oldObj.Spec.Sites {
		phase := projectedClusterPhase(&oldObj, site.Cluster)
		if phase != string(simplyblockv1alpha2.StorageClusterPhaseOnline) {
			return admission.Denied(fmt.Sprintf(
				"RoleSwapNotReady: the roles of stretched cluster %s cannot be swapped while site %s (%s) "+
					"reports its StorageCluster %s; a swap moves the cluster-wide operations between the "+
					"sites' operators and is admitted only while both sites are Online",
				oldObj.Name, site.Site, site.Cluster, orNotReported(phase)))
		}
	}
	return admission.Allowed("")
}

// backendRefusal reads the control plane's lost site and nodes, and refuses a
// swap while any node is not online with no site recorded lost: that is a site
// going down whose fence has not started. A control plane that cannot be read
// refuses rather than guesses. It returns the site the control plane records as
// lost and how far its fence got, for the caller to decide on.
func (v *StretchedClusterValidator) backendRefusal(
	ctx context.Context, sc *simplyblockv1alpha2.StretchedCluster, uuid string,
) (lost, record string, refusal *admission.Response) {
	deny := func(format string, args ...any) (string, string, *admission.Response) {
		r := admission.Denied("RoleSwapNotReady: " + fmt.Sprintf(format, args...))
		return "", "", &r
	}
	if v.Backend == nil {
		return deny("the roles of stretched cluster %s cannot be checked against the control plane, which this "+
			"operator does not reach", sc.Name)
	}
	cluster, err := v.Backend.Cluster(ctx, uuid)
	if err != nil {
		return deny("the control plane of stretched cluster %s cannot be read (%v); a swap is admitted only on "+
			"its current record of the sites", sc.Name, err)
	}
	if cluster.LostSite != "" {
		return cluster.LostSite, cluster.LostSiteState, nil
	}
	nodes, err := v.Backend.StorageNodes(ctx, uuid)
	if err != nil {
		return deny("the nodes of stretched cluster %s cannot be read from the control plane (%v)", sc.Name, err)
	}
	var down []string
	for _, n := range nodes {
		if n.Status != utils.NodeStatusOnline && n.Status != utils.NodeStatusRemoved {
			down = append(down, fmt.Sprintf("%s (site %s, %s)", n.UUID, n.Site, n.Status))
		}
	}
	if len(down) > 0 {
		return deny("node(s) %s of stretched cluster %s are not online, and the control plane records no site "+
			"as lost yet; a planned swap needs every node online, and a lost leader is handed over once the "+
			"control plane has fenced its site", strings.Join(down, ", "), sc.Name)
	}
	return "", "", nil
}

// fenceDone is the control plane's record of a lost site whose fence finished.
const fenceDone = "done"

// swapWithALostSite admits a swap away from a lost leader whose fence is done,
// to a survivor whose StorageCluster works, and nothing else while a site is
// recorded lost.
func swapWithALostSite(sc *simplyblockv1alpha2.StretchedCluster, lost, record string) admission.Response {
	var leader, survivor *simplyblockv1alpha2.StretchedSite
	for i := range sc.Spec.Sites {
		switch {
		case sc.Spec.Sites[i].Site == lost:
		case survivor == nil:
			survivor = &sc.Spec.Sites[i]
		}
		if sc.Spec.Sites[i].Role == simplyblockv1alpha2.StretchedRoleLeader {
			leader = &sc.Spec.Sites[i]
		}
	}
	if leader == nil || leader.Site != lost || survivor == nil {
		return admission.Denied(fmt.Sprintf(
			"RoleSwapNotReady: the control plane records site %s of stretched cluster %s as lost, "+
				"and a lost site cannot lead; the roles can be swapped only away from a lost leader", lost, sc.Name))
	}
	if record != fenceDone {
		return admission.Denied(fmt.Sprintf(
			"RoleSwapNotReady: the leader site %s of stretched cluster %s is recorded lost, but its fence is %s, "+
				"not done; until the control plane has fenced it, the old leader may still act, and a swap would "+
				"make two leaders", lost, sc.Name, orNotReported(record)))
	}
	switch phase := projectedClusterPhase(sc, survivor.Cluster); phase {
	case string(simplyblockv1alpha2.StorageClusterPhaseOnline), string(simplyblockv1alpha2.StorageClusterPhaseDegraded):
		return admission.Allowed("")
	default:
		return admission.Denied(fmt.Sprintf(
			"RoleSwapNotReady: site %s (%s) of stretched cluster %s reports its StorageCluster %s; "+
				"it can take the lead from the lost site %s only once it is Online or Degraded",
			survivor.Site, survivor.Cluster, sc.Name, orNotReported(phase), lost))
	}
}

// rolesSwapped reports whether any site's role changed.
func rolesSwapped(oldObj, newObj *simplyblockv1alpha2.StretchedCluster) bool {
	roles := map[string]simplyblockv1alpha2.StretchedRole{}
	for _, site := range oldObj.Spec.Sites {
		roles[site.Cluster] = site.Role
	}
	for _, site := range newObj.Spec.Sites {
		if was, ok := roles[site.Cluster]; ok && was != site.Role {
			return true
		}
	}
	return false
}

// projectedClusterPhase is the phase the hub last projected for a site's
// StorageCluster, empty when it has projected none.
func projectedClusterPhase(sc *simplyblockv1alpha2.StretchedCluster, cluster string) string {
	for _, s := range sc.Status.Sites {
		if s.Cluster == cluster && s.StorageCluster != nil {
			return s.StorageCluster.Phase
		}
	}
	return ""
}

func orNotReported(phase string) string {
	if phase == "" {
		return "not reported yet"
	}
	return phase
}
