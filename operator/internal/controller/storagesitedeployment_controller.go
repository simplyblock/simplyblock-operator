// The StorageSiteDeployment controller carries a managed site's storage
// deployment request from the hub to the site and projects the site's answer
// back.
//
// It never holds a site kubeconfig: every write to the site is a ManifestWork
// in the site's hub namespace, every read a ManagedClusterView there, the same
// two primitives the TestFailover controller uses. The work carries the
// OperatorOps discovery first; once the site has written a draft with nodes, it
// carries a server-side apply of the draft's sizing, and when the request is
// approved, the draft's approval. The views project the draft, the
// StorageCluster the approved draft expands into, and that cluster's nodes.
//
// The request withdraws nothing on deletion: the work is released with its
// resources orphaned, so a storage cluster is never torn down by deleting the
// request that asked for it. See docs/design/control-center-managed-discovery.md
// in the simplyblock-dr repository.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workv1 "open-cluster-management.io/api/work/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/pool"
)

// storageSiteDeploymentIDLabel tags the work and the views of one request, so
// its release can enumerate them.
const storageSiteDeploymentIDLabel = "storage.simplyblock.io/site-deployment"

// finalizerStorageSiteDeployment holds the request until its work and views
// are released. The work is released with its resources orphaned: the
// discovery, the draft and the storage cluster stay on the site.
const finalizerStorageSiteDeployment = "storage.simplyblock.io/storagesitedeployment-release"

// hubDeployFieldManager is the field manager the work agent applies the
// draft's sizing and approval with, so the discovery's own fields on the draft
// are left to their owner.
const hubDeployFieldManager = "hub-deploy"

// storageSiteDeploymentRequeue is how long the reconcile waits before reading
// the site's views again while the request is in progress.
const storageSiteDeploymentRequeue = 15 * time.Second

// storageSiteDeploymentOnlineRequeue keeps an Online request's projection of
// the storage cluster fresh without polling the site hard.
const storageSiteDeploymentOnlineRequeue = 2 * time.Minute

// maxStorageSiteNodeViews bounds the per-node views a request keeps: list
// views are not supported by ManagedClusterView, so there is one per node
// named in the draft's nodeRefs.
const maxStorageSiteNodeViews = 64

// annotationStorageClusterDefaultPool is the annotation the StorageCluster
// controller records the cluster's first pool under (controllers/cluster).
const annotationStorageClusterDefaultPool = "storage.simplyblock.io/default-pool"

// Conditions of a request.
const (
	ConditionStorageSiteDelivered  = "Delivered"
	ConditionStorageSiteDiscovered = "Discovered"
	ConditionStorageSiteApproved   = "Approved"
	ConditionStorageSiteReady      = "Ready"
)

// StorageSiteDeploymentReconciler reconciles a StorageSiteDeployment object.
type StorageSiteDeploymentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagesitedeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagesitedeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagesitedeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=work.open-cluster-management.io,resources=manifestworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=view.open-cluster-management.io,resources=managedclusterviews,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=managedclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile carries the request to the site and projects the site's answer:
// it ensures the finalizer and the work, reads the draft, the storage cluster
// and its nodes through views, and derives the phase from what they report.
func (r *StorageSiteDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var sd simplyblockv1alpha2.StorageSiteDeployment
	if err := r.Get(ctx, req.NamespacedName, &sd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sd.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, &sd)
	}
	if !controllerutil.ContainsFinalizer(&sd, finalizerStorageSiteDeployment) {
		controllerutil.AddFinalizer(&sd, finalizerStorageSiteDeployment)
		if err := r.Update(ctx, &sd); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	work, err := r.ensureWork(ctx, &sd)
	if err != nil {
		log.Error(err, "ensure ManifestWork")
		return ctrl.Result{}, err
	}
	return r.project(ctx, &sd, work)
}

// ensureWork creates or updates the request's ManifestWork: the discovery
// always, and the draft's sizing and approval once the site has a draft with
// nodes to apply them to.
func (r *StorageSiteDeploymentReconciler) ensureWork(ctx context.Context, sd *simplyblockv1alpha2.StorageSiteDeployment) (*workv1.ManifestWork, error) {
	want, err := r.manifestWork(sd)
	if err != nil {
		return nil, err
	}
	var have workv1.ManifestWork
	err = r.Get(ctx, client.ObjectKeyFromObject(want), &have)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, want); err != nil {
			return nil, err
		}
		return want, nil
	}
	if err != nil {
		return nil, err
	}
	if !apiequality.Semantic.DeepEqual(have.Spec, want.Spec) || !apiequality.Semantic.DeepEqual(have.Labels, want.Labels) {
		have.Spec = want.Spec
		have.Labels = want.Labels
		if err := r.Update(ctx, &have); err != nil {
			return nil, err
		}
	}
	return &have, nil
}

// manifestWork builds the request's work as it should be now. Its resources
// are orphaned on delete: the discovery, the draft and what it expanded into
// are the site's, and deleting the request must not take them away.
func (r *StorageSiteDeploymentReconciler) manifestWork(sd *simplyblockv1alpha2.StorageSiteDeployment) (*workv1.ManifestWork, error) {
	ns := siteNamespace(sd)
	draft := draftName(sd)
	labels := map[string]string{storageSiteDeploymentIDLabel: string(sd.UID)}

	ops := map[string]any{
		"apiVersion": simplyblockv1alpha2.GroupVersion.String(),
		"kind":       "OperatorOps",
		"metadata":   map[string]any{"name": discoveryName(sd), "namespace": ns, "labels": labels},
		"spec": map[string]any{
			"action":   string(simplyblockv1alpha2.OperatorOpsActionDiscover),
			"discover": discoverSpec(sd),
		},
	}
	manifests := []workv1.Manifest{}
	var configs []workv1.ManifestConfigOption
	raw, err := json.Marshal(ops)
	if err != nil {
		return nil, fmt.Errorf("marshal discovery: %w", err)
	}
	manifests = append(manifests, workv1.Manifest{RawExtension: runtime.RawExtension{Raw: raw}})

	// The sizing and the approval are applied onto the draft the discovery
	// wrote, never before it exists: an apply that created the draft would
	// make a document with no nodes, which the site refuses.
	if draftHasNodes(sd.Status.Draft) && (sd.Spec.Sizing != nil || sd.Spec.Approved) {
		spec := map[string]any{"approved": sd.Spec.Approved}
		if tpl := sizingTemplate(sd.Spec.Sizing); len(tpl) > 0 {
			spec["cluster"] = tpl
		}
		cdc := map[string]any{
			"apiVersion": simplyblockv1alpha2.GroupVersion.String(),
			"kind":       "ClusterDeploymentConfig",
			"metadata":   map[string]any{"name": draft, "namespace": ns},
			"spec":       spec,
		}
		raw, err := json.Marshal(cdc)
		if err != nil {
			return nil, fmt.Errorf("marshal draft apply: %w", err)
		}
		manifests = append(manifests, workv1.Manifest{RawExtension: runtime.RawExtension{Raw: raw}})
		configs = append(configs, workv1.ManifestConfigOption{
			ResourceIdentifier: workv1.ResourceIdentifier{
				Group: simplyblockv1alpha2.GroupVersion.Group, Resource: "clusterdeploymentconfigs", Namespace: ns, Name: draft,
			},
			UpdateStrategy: &workv1.UpdateStrategy{
				Type: workv1.UpdateStrategyTypeServerSideApply,
				ServerSideApply: &workv1.ServerSideApplyConfig{
					Force:        true,
					FieldManager: hubDeployFieldManager,
				},
			},
			FeedbackRules: []workv1.FeedbackRule{{
				Type: workv1.JSONPathsType,
				JsonPaths: []workv1.JsonPath{
					{Name: "phase", Path: ".status.phase"},
					{Name: "approved", Path: ".spec.approved"},
				},
			}},
		})
	}

	return &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workName(sd),
			Namespace: sd.Spec.Cluster,
			Labels:    labels,
		},
		Spec: workv1.ManifestWorkSpec{
			Workload:        workv1.ManifestsTemplate{Manifests: manifests},
			ManifestConfigs: configs,
			DeleteOption:    &workv1.DeleteOption{PropagationPolicy: workv1.DeletePropagationPolicyTypeOrphan},
		},
	}, nil
}

// discoverSpec is the OperatorOps discover block of the request.
func discoverSpec(sd *simplyblockv1alpha2.StorageSiteDeployment) map[string]any {
	d := map[string]any{"configName": draftName(sd)}
	if sd.Spec.Discover.EnableControlPlaneNodes != nil {
		d["enableControlPlaneNodes"] = *sd.Spec.Discover.EnableControlPlaneNodes
	}
	if len(sd.Spec.Discover.Workers) > 0 {
		d["workers"] = sd.Spec.Discover.Workers
	}
	if len(sd.Spec.Discover.NodeSelector) > 0 {
		d["nodeSelector"] = sd.Spec.Discover.NodeSelector
	}
	return d
}

// sizingTemplate is the draft's cluster template fields the request sets.
// Only the stated fields are applied, so what the discovery wrote stays.
func sizingTemplate(s *simplyblockv1alpha2.StorageSiteSizing) map[string]any {
	tpl := map[string]any{}
	if s == nil {
		return tpl
	}
	if s.Name != "" {
		tpl["name"] = s.Name
	}
	if s.VCPUCount != nil {
		tpl["vcpuCount"] = *s.VCPUCount
	}
	if s.MinHugePagesSize != "" {
		tpl["minHugePagesSize"] = s.MinHugePagesSize
	}
	if s.MaxSubsystemCount != nil {
		tpl["maxSubsystemCount"] = *s.MaxSubsystemCount
	}
	if s.EnableDriveFormat != nil {
		tpl["enableDriveFormat"] = *s.EnableDriveFormat
	}
	if s.EnableJournalDevice != nil {
		tpl["enableJournalDevice"] = *s.EnableJournalDevice
	}
	if s.Stripe != nil {
		stripe := map[string]any{}
		if s.Stripe.DataChunks != nil {
			stripe["dataChunks"] = *s.Stripe.DataChunks
		}
		if s.Stripe.ParityChunks != nil {
			stripe["parityChunks"] = *s.Stripe.ParityChunks
		}
		tpl["stripe"] = stripe
	}
	return tpl
}

// project reads the site's views and derives the request's phase.
func (r *StorageSiteDeploymentReconciler) project(ctx context.Context, sd *simplyblockv1alpha2.StorageSiteDeployment, work *workv1.ManifestWork) (ctrl.Result, error) {
	delivered, deliveryMessage := workDelivery(work)

	draftObj, haveDraft, err := r.projected(ctx, sd, "draft", "clusterdeploymentconfigs", draftName(sd), siteNamespace(sd))
	if err != nil {
		return ctrl.Result{}, err
	}
	if !haveDraft {
		if deliveryMessage != "" {
			return r.setPhase(ctx, sd, simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed, deliveryMessage, func(s *simplyblockv1alpha2.StorageSiteDeploymentStatus) {
				setCondition(s, ConditionStorageSiteDelivered, false, "NotApplied", deliveryMessage)
			})
		}
		return r.setPhase(ctx, sd, simplyblockv1alpha2.StorageSiteDeploymentPhaseDiscovering,
			fmt.Sprintf("waiting for site %s to write draft %s/%s", sd.Spec.Cluster, siteNamespace(sd), draftName(sd)),
			func(s *simplyblockv1alpha2.StorageSiteDeploymentStatus) {
				s.WorkName = work.Name
				setCondition(s, ConditionStorageSiteDelivered, delivered, deliveryReason(delivered), deliveryNote(delivered, deliveryMessage))
			})
	}

	var cdc simplyblockv1alpha2.ClusterDeploymentConfig
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(draftObj, &cdc); err != nil {
		return r.setPhase(ctx, sd, simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed,
			fmt.Sprintf("the site's draft could not be read: %v", err), nil)
	}
	draft := projectDraft(&cdc)
	base := func(s *simplyblockv1alpha2.StorageSiteDeploymentStatus) {
		s.WorkName = work.Name
		s.Draft = draft
		setCondition(s, ConditionStorageSiteDelivered, delivered, deliveryReason(delivered), deliveryNote(delivered, deliveryMessage))
		setCondition(s, ConditionStorageSiteDiscovered, draftHasNodes(draft), "Nodes", fmt.Sprintf("%d node(s) in the draft", draftNodeCount(draft)))
		setCondition(s, ConditionStorageSiteApproved, draft.Approved, "SiteDraft", fmt.Sprintf("the site's draft approved=%t", draft.Approved))
	}

	if !draftHasNodes(draft) {
		return r.setPhase(ctx, sd, simplyblockv1alpha2.StorageSiteDeploymentPhaseDiscovering,
			"the site's draft names no node yet: discovery is running", base)
	}
	if deliveryMessage != "" {
		// The draft exists, so the message is about the sizing or the approval
		// the work could not apply: the site refused it.
		return r.setPhase(ctx, sd, simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed, deliveryMessage, base)
	}
	switch {
	case cdc.Status.Phase == simplyblockv1alpha2.ClusterDeploymentConfigPhaseFailed:
		return r.setPhase(ctx, sd, simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed,
			"the site's draft failed: "+orDefault(cdc.Status.Message, "no message"), base)
	case !cdc.Spec.Approved:
		msg := "the draft awaits approval"
		if sd.Spec.Approved {
			msg = "approval requested; waiting for the site's draft to take it"
		} else if sd.Spec.Sizing != nil && !sizingApplied(sd.Spec.Sizing, cdc.Spec.Cluster) {
			msg = "the draft awaits approval; the sizing is being applied"
		}
		return r.setPhase(ctx, sd, simplyblockv1alpha2.StorageSiteDeploymentPhaseDrafted, msg, base)
	}

	// Approved on the site: follow the StorageCluster it expands into.
	clusterName := cdc.Status.ClusterRef
	if clusterName == "" && cdc.Spec.Cluster != nil {
		clusterName = cdc.Spec.Cluster.Name
	}
	if clusterName == "" {
		return r.setPhase(ctx, sd, simplyblockv1alpha2.StorageSiteDeploymentPhaseDeploying,
			"the draft is approved; waiting for the site to name its StorageCluster", base)
	}
	scObj, haveSC, err := r.projected(ctx, sd, "cluster", "storageclusters", clusterName, siteNamespace(sd))
	if err != nil {
		return ctrl.Result{}, err
	}
	sc := &simplyblockv1alpha2.StorageSiteCluster{Name: clusterName}
	if haveSC {
		var cluster simplyblockv1alpha2.StorageCluster
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(scObj, &cluster); err == nil {
			sc.UUID = cluster.Status.UUID
			sc.Phase = string(cluster.Status.Phase)
			sc.Pool = cluster.Annotations[annotationStorageClusterDefaultPool]
		}
	}
	if sc.Pool == "" {
		sc.Pool = pool.DefaultPoolName(clusterName)
	}
	nodes, err := r.projectNodes(ctx, sd, draft.NodeRefs)
	if err != nil {
		return ctrl.Result{}, err
	}
	sc.Nodes = nodes
	withCluster := func(s *simplyblockv1alpha2.StorageSiteDeploymentStatus) {
		base(s)
		s.StorageCluster = sc
	}

	switch {
	case sc.Phase == string(simplyblockv1alpha2.StorageClusterPhaseOnline) && sc.UUID != "":
		return r.setPhase(ctx, sd, simplyblockv1alpha2.StorageSiteDeploymentPhaseOnline,
			fmt.Sprintf("StorageCluster %s is Online (%d node(s))", clusterName, len(nodes)), func(s *simplyblockv1alpha2.StorageSiteDeploymentStatus) {
				withCluster(s)
				setCondition(s, ConditionStorageSiteReady, true, "Online", "the StorageCluster is Online")
			})
	case cdc.Status.Phase == simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanded && sc.Phase == string(simplyblockv1alpha2.StorageClusterPhaseUnavailable):
		return r.setPhase(ctx, sd, simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed,
			fmt.Sprintf("StorageCluster %s is Unavailable after the expansion", clusterName), withCluster)
	}
	msg := fmt.Sprintf("draft %s, StorageCluster %s %s", orDefault(string(cdc.Status.Phase), "Expanding"), clusterName, orDefault(sc.Phase, "not reported yet"))
	if step := cdc.Status.Step.State; step != "" && cdc.Status.Phase == simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding {
		msg = fmt.Sprintf("draft Expanding (%s), StorageCluster %s %s", step, clusterName, orDefault(sc.Phase, "not reported yet"))
	}
	return r.setPhase(ctx, sd, simplyblockv1alpha2.StorageSiteDeploymentPhaseDeploying, msg, func(s *simplyblockv1alpha2.StorageSiteDeploymentStatus) {
		withCluster(s)
		setCondition(s, ConditionStorageSiteReady, false, "Deploying", msg)
	})
}

// projectNodes projects the draft's StorageNodes, one view each.
func (r *StorageSiteDeploymentReconciler) projectNodes(ctx context.Context, sd *simplyblockv1alpha2.StorageSiteDeployment, refs []string) ([]simplyblockv1alpha2.StorageSiteNode, error) {
	sorted := append([]string(nil), refs...)
	sort.Strings(sorted)
	if len(sorted) > maxStorageSiteNodeViews {
		sorted = sorted[:maxStorageSiteNodeViews]
	}
	nodes := make([]simplyblockv1alpha2.StorageSiteNode, 0, len(sorted))
	for i, name := range sorted {
		obj, ok, err := r.projected(ctx, sd, fmt.Sprintf("node-%d", i), "storagenodes", name, siteNamespace(sd))
		if err != nil {
			return nil, err
		}
		n := simplyblockv1alpha2.StorageSiteNode{Name: name}
		if ok {
			var sn simplyblockv1alpha2.StorageNode
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &sn); err == nil {
				n.Phase = string(sn.Status.Phase)
				n.Hostname = sn.Status.Hostname
			}
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// projected reads one of the request's views, creating it when it is missing,
// and reports whether the site has projected the object yet.
func (r *StorageSiteDeploymentReconciler) projected(ctx context.Context, sd *simplyblockv1alpha2.StorageSiteDeployment, suffix, resource, name, namespace string) (map[string]interface{}, bool, error) {
	viewName := storageSiteViewName(sd, suffix)
	view := &unstructured.Unstructured{}
	view.SetGroupVersionKind(managedClusterViewGVK)
	getErr := r.Get(ctx, client.ObjectKey{Namespace: sd.Spec.Cluster, Name: viewName}, view)
	if apierrors.IsNotFound(getErr) {
		want := newStorageSiteView(sd, viewName, resource, name, namespace)
		if createErr := r.Create(ctx, want); createErr != nil {
			return nil, false, createErr
		}
		return nil, false, nil
	}
	if getErr != nil {
		return nil, false, getErr
	}
	// A view that names another object (the draft's cluster changed) is
	// pointed at the right one.
	scope, _, _ := unstructured.NestedMap(view.Object, "spec", "scope")
	if scope["name"] != name || scope["resource"] != resource {
		want := newStorageSiteView(sd, viewName, resource, name, namespace)
		view.Object["spec"] = want.Object["spec"]
		if err := r.Update(ctx, view); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	result, found, nestedErr := unstructured.NestedMap(view.Object, "status", "result")
	if nestedErr != nil || !found || len(result) == 0 {
		return nil, false, nil
	}
	return result, true, nil
}

// newStorageSiteView asks the site to project one object back to the hub.
func newStorageSiteView(sd *simplyblockv1alpha2.StorageSiteDeployment, name, resource, targetName, targetNamespace string) *unstructured.Unstructured {
	scope := map[string]interface{}{"resource": resource, "name": targetName}
	if targetNamespace != "" {
		scope["namespace"] = targetNamespace
	}
	view := &unstructured.Unstructured{}
	view.SetGroupVersionKind(managedClusterViewGVK)
	view.SetNamespace(sd.Spec.Cluster)
	view.SetName(name)
	view.SetLabels(map[string]string{storageSiteDeploymentIDLabel: string(sd.UID)})
	_ = unstructured.SetNestedMap(view.Object, scope, "spec", "scope")
	return view
}

// setPhase writes the phase, the message and the mutation, and records a
// phase change as an event.
func (r *StorageSiteDeploymentReconciler) setPhase(ctx context.Context, sd *simplyblockv1alpha2.StorageSiteDeployment, phase simplyblockv1alpha2.StorageSiteDeploymentPhase, message string, mutate func(*simplyblockv1alpha2.StorageSiteDeploymentStatus)) (ctrl.Result, error) {
	previous := sd.Status.Phase
	if err := r.patchStatus(ctx, sd, func(s *simplyblockv1alpha2.StorageSiteDeploymentStatus) {
		if mutate != nil {
			mutate(s)
		}
		s.Phase = phase
		s.Message = message
	}); err != nil {
		return ctrl.Result{}, err
	}
	if previous != phase && r.Recorder != nil {
		kind := corev1.EventTypeNormal
		if phase == simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed {
			kind = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(sd, nil, kind, string(phase), string(phase), "%s", message)
	}
	switch phase {
	case simplyblockv1alpha2.StorageSiteDeploymentPhaseOnline:
		return ctrl.Result{RequeueAfter: storageSiteDeploymentOnlineRequeue}, nil
	case simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed:
		// A failure on the site may clear (a node comes back, a draft is
		// corrected on the site): keep reading at the slow cadence.
		return ctrl.Result{RequeueAfter: storageSiteDeploymentOnlineRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: storageSiteDeploymentRequeue}, nil
}

// patchStatus applies mutate to the status and writes it with the generation
// it was computed from.
func (r *StorageSiteDeploymentReconciler) patchStatus(ctx context.Context, sd *simplyblockv1alpha2.StorageSiteDeployment, mutate func(*simplyblockv1alpha2.StorageSiteDeploymentStatus)) error {
	base := client.MergeFrom(sd.DeepCopy())
	mutate(&sd.Status)
	sd.Status.ObservedGeneration = sd.Generation
	return r.Status().Patch(ctx, sd, base)
}

// reconcileDeletion releases the request's work and views and removes the
// finalizer. The work orphans its resources, so nothing on the site goes.
func (r *StorageSiteDeploymentReconciler) reconcileDeletion(ctx context.Context, sd *simplyblockv1alpha2.StorageSiteDeployment) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sd, finalizerStorageSiteDeployment) {
		return ctrl.Result{}, nil
	}
	var work workv1.ManifestWork
	err := r.Get(ctx, client.ObjectKey{Namespace: sd.Spec.Cluster, Name: workName(sd)}, &work)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return ctrl.Result{}, err
	case work.DeletionTimestamp.IsZero():
		if err := client.IgnoreNotFound(r.Delete(ctx, &work)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	default:
		// Deleting: wait for the work agent to release it.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	views := &unstructured.UnstructuredList{}
	listGVK := managedClusterViewGVK
	listGVK.Kind += listKindSuffix
	views.SetGroupVersionKind(listGVK)
	if err := r.List(ctx, views, client.InNamespace(sd.Spec.Cluster), client.MatchingLabels{storageSiteDeploymentIDLabel: string(sd.UID)}); err != nil && !meta.IsNoMatchError(err) {
		return ctrl.Result{}, err
	}
	for i := range views.Items {
		if err := client.IgnoreNotFound(r.Delete(ctx, &views.Items[i])); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(sd, finalizerStorageSiteDeployment)
	return ctrl.Result{}, r.Update(ctx, sd)
}

// workDelivery reads the work's status: whether every manifest is applied,
// and the first manifest's refusal when one is not.
func workDelivery(work *workv1.ManifestWork) (applied bool, message string) {
	if work == nil {
		return false, ""
	}
	for _, m := range work.Status.ResourceStatus.Manifests {
		for _, c := range m.Conditions {
			if c.Type == workv1.ManifestApplied && c.Status == metav1.ConditionFalse {
				return false, fmt.Sprintf("the site did not apply %s %s: %s", m.ResourceMeta.Kind, m.ResourceMeta.Name, c.Message)
			}
		}
	}
	for _, c := range work.Status.Conditions {
		if c.Type == workv1.WorkApplied {
			return c.Status == metav1.ConditionTrue, ""
		}
	}
	return false, ""
}

func deliveryReason(delivered bool) string {
	if delivered {
		return "Applied"
	}
	return "Pending"
}

func deliveryNote(delivered bool, message string) string {
	switch {
	case message != "":
		return message
	case delivered:
		return "the work is applied on the site"
	}
	return "the work is not applied on the site yet"
}

// projectDraft is the draft as the status carries it.
func projectDraft(cdc *simplyblockv1alpha2.ClusterDeploymentConfig) *simplyblockv1alpha2.StorageSiteDraft {
	d := &simplyblockv1alpha2.StorageSiteDraft{
		Name:     cdc.Name,
		Phase:    string(cdc.Status.Phase),
		Message:  cdc.Status.Message,
		Approved: cdc.Spec.Approved,
		NodeSets: cdc.Spec.NodeSets,
		NodeRefs: cdc.Status.NodeRefs,
	}
	if cdc.Spec.Cluster != nil {
		d.Cluster = cdc.Spec.Cluster.DeepCopy()
	}
	if d.Phase == "" {
		d.Phase = string(simplyblockv1alpha2.ClusterDeploymentConfigPhaseDraft)
	}
	return d
}

// draftHasNodes is whether the draft names at least one worker.
func draftHasNodes(d *simplyblockv1alpha2.StorageSiteDraft) bool {
	return draftNodeCount(d) > 0
}

func draftNodeCount(d *simplyblockv1alpha2.StorageSiteDraft) int {
	if d == nil {
		return 0
	}
	n := 0
	for _, set := range d.NodeSets {
		for _, g := range set.Groups {
			n += len(g.Workers)
		}
	}
	return n
}

// sizingApplied is whether the draft's template carries the request's sizing.
func sizingApplied(s *simplyblockv1alpha2.StorageSiteSizing, tpl *simplyblockv1alpha2.ClusterTemplate) bool {
	if s == nil {
		return true
	}
	if tpl == nil {
		return false
	}
	eq32 := func(want, have *int32) bool { return want == nil || (have != nil && *have == *want) }
	eqBool := func(want, have *bool) bool { return want == nil || (have != nil && *have == *want) }
	if s.Name != "" && tpl.Name != s.Name {
		return false
	}
	if s.MinHugePagesSize != "" && tpl.MinHugePagesSize != s.MinHugePagesSize {
		return false
	}
	if !eq32(s.VCPUCount, tpl.VCPUCount) || !eq32(s.MaxSubsystemCount, tpl.MaxSubsystemCount) ||
		!eqBool(s.EnableDriveFormat, tpl.EnableDriveFormat) || !eqBool(s.EnableJournalDevice, tpl.EnableJournalDevice) {
		return false
	}
	if s.Stripe != nil {
		if tpl.Stripe == nil || !eq32(s.Stripe.DataChunks, tpl.Stripe.DataChunks) || !eq32(s.Stripe.ParityChunks, tpl.Stripe.ParityChunks) {
			return false
		}
	}
	return true
}

func setCondition(s *simplyblockv1alpha2.StorageSiteDeploymentStatus, kind string, ok bool, reason, message string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: s.ObservedGeneration})
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func siteNamespace(sd *simplyblockv1alpha2.StorageSiteDeployment) string {
	return orDefault(sd.Spec.SiteNamespace, "simplyblock")
}

func draftName(sd *simplyblockv1alpha2.StorageSiteDeployment) string {
	return orDefault(sd.Spec.DraftName, "site-draft")
}

// storageSiteHash is a short, deterministic id of the request for the names
// of its work and views, which must be bounded and found again on a restart.
func storageSiteHash(sd *simplyblockv1alpha2.StorageSiteDeployment) string {
	h := sha256.Sum256([]byte(sd.Namespace + "/" + sd.Name))
	return fmt.Sprintf("%x", h[:6])
}

func workName(sd *simplyblockv1alpha2.StorageSiteDeployment) string {
	return "sbsd-" + storageSiteHash(sd)
}

func storageSiteViewName(sd *simplyblockv1alpha2.StorageSiteDeployment, suffix string) string {
	return "sbsd-" + storageSiteHash(sd) + "-" + suffix
}

// discoveryName is the OperatorOps on the site. It carries a hash of the
// discovery's parameters, so a changed discovery is a new run rather than an
// edit of a finished one.
func discoveryName(sd *simplyblockv1alpha2.StorageSiteDeployment) string {
	raw, _ := json.Marshal(sd.Spec.Discover)
	h := sha256.Sum256(raw)
	return fmt.Sprintf("hub-discover-%s-%x", draftName(sd), h[:3])
}

// SetupWithManager registers the controller. The work and the views live in
// the site's namespace on the hub, where an owner reference to the request
// cannot point, so the site's answers are read on the requeue cadence of the
// phase rather than through a watch.
func (r *StorageSiteDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageSiteDeployment{}).
		Named("storagesitedeployment").
		Complete(r)
}

// listKindSuffix turns a kind into its list kind (StorageCluster ->
// StorageClusterList) for an unstructured list read.
const listKindSuffix = "List"
