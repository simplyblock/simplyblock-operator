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

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-stretchedcluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=stretchedclusters,verbs=update,versions=v1alpha2,name=vstretchedcluster.simplyblock.io,admissionReviewVersions=v1

// StretchedClusterValidator refuses a role swap a site is not ready for.
//
// It holds no client: whether both sites are Online is what the request's own
// status projects, read from the object being updated.
type StretchedClusterValidator struct{}

func (v *StretchedClusterValidator) Handle(_ context.Context, req admission.Request) admission.Response {
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
	if lost := oldObj.Status.LostSite; lost != "" {
		return swapWithALostSite(&oldObj, lost)
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

// fenceDone is the control plane's record of a lost site whose fence finished.
const fenceDone = "done"

// swapWithALostSite admits a swap away from a lost leader whose fence is done,
// to a survivor whose StorageCluster works, and nothing else while a site is
// recorded lost.
func swapWithALostSite(sc *simplyblockv1alpha2.StretchedCluster, lost string) admission.Response {
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
	record := siteRecordOf(sc, survivor.Cluster)
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

// siteRecordOf is the control plane's record of the lost site as the given
// site's StorageCluster reports it.
func siteRecordOf(sc *simplyblockv1alpha2.StretchedCluster, cluster string) string {
	for _, s := range sc.Status.Sites {
		if s.Cluster == cluster {
			return s.SiteRecord
		}
	}
	return ""
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
