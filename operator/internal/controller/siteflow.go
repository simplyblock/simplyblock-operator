// One managed site's deployment flow, as the hub drives it: a ManifestWork that
// carries a discovery to the site and, once the site's draft names nodes, what
// the hub applies onto that draft; and ManagedClusterViews that project the
// draft and the StorageCluster it expands into back to the hub. The flow reads
// the site and reports where it stands; the caller writes the status, so one
// caller can run it for one site and another for two.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	workv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/pool"
)

// siteFlow is one site's request.
type siteFlow struct {
	// ManagedCluster is the OCM ManagedCluster, whose namespace on the hub holds
	// the work and the views.
	ManagedCluster string
	// SiteNamespace is the operator's namespace on the site.
	SiteNamespace string
	// DraftName is the ClusterDeploymentConfig the discovery writes.
	DraftName string
	// Discover is the discovery the site runs.
	Discover simplyblockv1alpha2.StorageSiteDiscovery
	// Labels identify the requester on the work, the discovery, and the views.
	Labels map[string]string
	// WorkName names the work; the views are WorkName-<suffix>.
	WorkName string
	// DraftSpec is applied onto the draft by server-side apply, nil for
	// nothing yet. It is applied only once PreviousDraft names nodes: an apply
	// that created the draft would make a document with no nodes, which the site
	// refuses.
	DraftSpec map[string]any
	// PreviousDraft is the draft as the requester's status last recorded it.
	PreviousDraft *simplyblockv1alpha2.StorageSiteDraft
	// Extra are further objects the work delivers, as unstructured maps.
	Extra []map[string]any
}

// siteReading is where one site stands.
type siteReading struct {
	Phase          simplyblockv1alpha2.StorageSiteDeploymentPhase
	Message        string
	WorkName       string
	Delivered      bool
	Draft          *simplyblockv1alpha2.StorageSiteDraft
	StorageCluster *simplyblockv1alpha2.StorageSiteCluster
	// SiteRecord is the control plane's record of a lost site as the site's
	// StorageCluster reports it, empty while both sites serve.
	SiteRecord string
	// LostSite is the site that record is about, as the site reports it.
	LostSite string
}

// discoveryName is the OperatorOps on the site, carrying a hash of the
// discovery's parameters so a changed discovery is a new run rather than an
// edit of a finished one.
func (f siteFlow) discoveryName() string {
	return discoveryNameFor(f.DraftName, f.Discover)
}

func discoveryNameFor(draft string, discover simplyblockv1alpha2.StorageSiteDiscovery) string {
	raw, _ := json.Marshal(discover)
	return fmt.Sprintf("hub-discover-%s-%x", draft, sha256Prefix(raw, 3))
}

func (f siteFlow) discoverSpec() map[string]any {
	d := map[string]any{"configName": f.DraftName}
	if f.Discover.EnableControlPlaneNodes != nil {
		d["enableControlPlaneNodes"] = *f.Discover.EnableControlPlaneNodes
	}
	if len(f.Discover.Workers) > 0 {
		d["workers"] = f.Discover.Workers
	}
	if len(f.Discover.NodeSelector) > 0 {
		d["nodeSelector"] = f.Discover.NodeSelector
	}
	return d
}

// work is the site's ManifestWork as it should be now. Its resources are
// orphaned on delete: what the site built is the site's.
func (f siteFlow) work() (*workv1.ManifestWork, error) {
	objects := []map[string]any{{
		"apiVersion": simplyblockv1alpha2.GroupVersion.String(),
		"kind":       "OperatorOps",
		"metadata":   map[string]any{"name": f.discoveryName(), "namespace": f.SiteNamespace, "labels": f.Labels},
		"spec": map[string]any{
			"action":   string(simplyblockv1alpha2.OperatorOpsActionDiscover),
			"discover": f.discoverSpec(),
		},
	}}
	var configs []workv1.ManifestConfigOption
	if f.DraftSpec != nil && draftHasNodes(f.PreviousDraft) {
		objects = append(objects, map[string]any{
			"apiVersion": simplyblockv1alpha2.GroupVersion.String(),
			"kind":       "ClusterDeploymentConfig",
			"metadata":   map[string]any{"name": f.DraftName, "namespace": f.SiteNamespace},
			"spec":       f.DraftSpec,
		})
		configs = append(configs, workv1.ManifestConfigOption{
			ResourceIdentifier: workv1.ResourceIdentifier{
				Group: simplyblockv1alpha2.GroupVersion.Group, Resource: "clusterdeploymentconfigs",
				Namespace: f.SiteNamespace, Name: f.DraftName,
			},
			UpdateStrategy: &workv1.UpdateStrategy{
				Type: workv1.UpdateStrategyTypeServerSideApply,
				ServerSideApply: &workv1.ServerSideApplyConfig{
					Force: true, FieldManager: hubDeployFieldManager,
				},
			},
		})
	}
	objects = append(objects, f.Extra...)

	manifests := make([]workv1.Manifest, 0, len(objects))
	for _, object := range objects {
		raw, err := json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("marshal %v: %w", object["kind"], err)
		}
		manifests = append(manifests, workv1.Manifest{RawExtension: runtime.RawExtension{Raw: raw}})
	}
	return &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{Name: f.WorkName, Namespace: f.ManagedCluster, Labels: f.Labels},
		Spec: workv1.ManifestWorkSpec{
			Workload:        workv1.ManifestsTemplate{Manifests: manifests},
			ManifestConfigs: configs,
			DeleteOption:    &workv1.DeleteOption{PropagationPolicy: workv1.DeletePropagationPolicyTypeOrphan},
		},
	}, nil
}

// ensureWork creates or updates the site's work.
func (f siteFlow) ensureWork(ctx context.Context, c client.Client) (*workv1.ManifestWork, error) {
	want, err := f.work()
	if err != nil {
		return nil, err
	}
	var have workv1.ManifestWork
	err = c.Get(ctx, client.ObjectKeyFromObject(want), &have)
	if apierrors.IsNotFound(err) {
		return want, c.Create(ctx, want)
	}
	if err != nil {
		return nil, err
	}
	if !apiequality.Semantic.DeepEqual(have.Spec, want.Spec) || !apiequality.Semantic.DeepEqual(have.Labels, want.Labels) {
		have.Spec = want.Spec
		have.Labels = want.Labels
		if err := c.Update(ctx, &have); err != nil {
			return nil, err
		}
	}
	return &have, nil
}

// read projects the site's draft and StorageCluster and reports where the site
// stands, in the phases a StorageSiteDeployment reports for one site.
func (f siteFlow) read(ctx context.Context, c client.Client, work *workv1.ManifestWork) (siteReading, error) {
	delivered, refusal := workDelivery(work)
	reading := siteReading{WorkName: work.Name, Delivered: delivered}

	draftObj, haveDraft, err := f.projected(ctx, c, "draft", "clusterdeploymentconfigs", f.DraftName)
	if err != nil {
		return reading, err
	}
	if !haveDraft {
		if refusal != "" {
			return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed, refusal), nil
		}
		return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseDiscovering, fmt.Sprintf(
			"waiting for site %s to write draft %s/%s", f.ManagedCluster, f.SiteNamespace, f.DraftName)), nil
	}

	var cdc simplyblockv1alpha2.ClusterDeploymentConfig
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(draftObj, &cdc); err != nil {
		return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed,
			fmt.Sprintf("the site's draft could not be read: %v", err)), nil
	}
	reading.Draft = projectDraft(&cdc)

	switch {
	case !draftHasNodes(reading.Draft):
		return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseDiscovering,
			"the site's draft names no node yet: discovery is running"), nil
	case refusal != "" && !cdc.Spec.Approved:
		// The draft exists, so the refusal is of what the hub applied onto it.
		return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed, refusal), nil
	case cdc.Status.Phase == simplyblockv1alpha2.ClusterDeploymentConfigPhaseFailed:
		return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed,
			"the site's draft failed: "+orDefault(cdc.Status.Message, "no message")), nil
	case !cdc.Spec.Approved:
		return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseDrafted, "the draft awaits approval"), nil
	}

	clusterName := cdc.Status.ClusterRef
	if clusterName == "" && cdc.Spec.Cluster != nil {
		clusterName = cdc.Spec.Cluster.Name
	}
	if clusterName == "" {
		return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseDeploying,
			"the draft is approved; waiting for the site to name its StorageCluster"), nil
	}
	scObj, haveSC, err := f.projected(ctx, c, "cluster", "storageclusters", clusterName)
	if err != nil {
		return reading, err
	}
	sc := &simplyblockv1alpha2.StorageSiteCluster{Name: clusterName}
	// heldBy is the StorageCluster's own status message: while it is still being
	// created, the reason a step holds it, such as a failed preflight (§5.4).
	var heldBy string
	if haveSC {
		var cluster simplyblockv1alpha2.StorageCluster
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(scObj, &cluster); err == nil {
			sc.UUID = cluster.Status.UUID
			sc.Phase = string(cluster.Status.Phase)
			heldBy = cluster.Status.Message
			sc.Pool = cluster.Annotations[annotationStorageClusterDefaultPool]
			if cluster.Status.Stretched != nil {
				reading.SiteRecord = cluster.Status.Stretched.SiteRecord
				reading.LostSite = cluster.Status.Stretched.LostSite
			}
		}
	}
	if sc.Pool == "" {
		sc.Pool = pool.DefaultPoolName(clusterName)
	}
	reading.StorageCluster = sc

	// A site that refused what the hub re-applied onto its approved draft still
	// runs the cluster it has, and the request keeps projecting it: the role-swap
	// guard reads it, and a swap that failed has to be possible to swap back.
	if refusal != "" {
		return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed, refusal), nil
	}

	switch {
	case sc.Phase == string(simplyblockv1alpha2.StorageClusterPhaseOnline) && sc.UUID != "":
		return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseOnline,
			fmt.Sprintf("StorageCluster %s is Online", clusterName)), nil
	case cdc.Status.Phase == simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanded &&
		sc.Phase == string(simplyblockv1alpha2.StorageClusterPhaseUnavailable):
		return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed,
			fmt.Sprintf("StorageCluster %s is Unavailable after the expansion", clusterName)), nil
	}
	message := fmt.Sprintf("draft %s, StorageCluster %s %s", orDefault(string(cdc.Status.Phase), "Expanding"),
		clusterName, orDefault(sc.Phase, "not reported yet"))
	if heldBy != "" {
		message += ": " + heldBy
	}
	return reading.at(simplyblockv1alpha2.StorageSiteDeploymentPhaseDeploying, message), nil
}

func (r siteReading) at(phase simplyblockv1alpha2.StorageSiteDeploymentPhase, message string) siteReading {
	r.Phase = phase
	r.Message = message
	return r
}

// projected reads one of the site's views, creating it when it is missing or
// re-pointing it when it names another object, and reports whether the site has
// projected the object yet.
func (f siteFlow) projected(
	ctx context.Context, c client.Client, suffix, resource, name string,
) (map[string]interface{}, bool, error) {
	viewName := f.WorkName + "-" + suffix
	scope := map[string]interface{}{"resource": resource, "name": name, "namespace": f.SiteNamespace}
	view := &unstructured.Unstructured{}
	view.SetGroupVersionKind(managedClusterViewGVK)
	err := c.Get(ctx, client.ObjectKey{Namespace: f.ManagedCluster, Name: viewName}, view)
	if apierrors.IsNotFound(err) {
		want := &unstructured.Unstructured{}
		want.SetGroupVersionKind(managedClusterViewGVK)
		want.SetNamespace(f.ManagedCluster)
		want.SetName(viewName)
		want.SetLabels(f.Labels)
		_ = unstructured.SetNestedMap(want.Object, scope, "spec", "scope")
		return nil, false, c.Create(ctx, want)
	}
	if err != nil {
		return nil, false, err
	}
	have, _, _ := unstructured.NestedMap(view.Object, "spec", "scope")
	if have["name"] != name || have["resource"] != resource {
		_ = unstructured.SetNestedMap(view.Object, scope, "spec", "scope")
		return nil, false, c.Update(ctx, view)
	}
	result, found, nestedErr := unstructured.NestedMap(view.Object, "status", "result")
	if nestedErr != nil || !found || len(result) == 0 {
		return nil, false, nil
	}
	return result, true, nil
}

// release deletes the site's work and every view labeled for the requester. It
// reports done once the work is gone; the work agent releases it on its own
// time, and the caller waits.
func (f siteFlow) release(ctx context.Context, c client.Client) (bool, error) {
	var work workv1.ManifestWork
	err := c.Get(ctx, client.ObjectKey{Namespace: f.ManagedCluster, Name: f.WorkName}, &work)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return false, err
	case work.DeletionTimestamp.IsZero():
		return false, client.IgnoreNotFound(c.Delete(ctx, &work))
	default:
		return false, nil
	}
	views := &unstructured.UnstructuredList{}
	listGVK := managedClusterViewGVK
	listGVK.Kind += listKindSuffix
	views.SetGroupVersionKind(listGVK)
	if err := c.List(ctx, views, client.InNamespace(f.ManagedCluster),
		client.MatchingLabels(f.Labels)); err != nil && !meta.IsNoMatchError(err) {
		return false, err
	}
	for i := range views.Items {
		if err := client.IgnoreNotFound(c.Delete(ctx, &views.Items[i])); err != nil {
			return false, err
		}
	}
	return true, nil
}

// sha256Prefix is the first n bytes of the SHA-256 of raw.
func sha256Prefix(raw []byte, n int) []byte {
	h := sha256.Sum256(raw)
	return h[:n]
}
