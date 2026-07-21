/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"

	"strconv"
	"strings"

	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/tlsutil"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// StorageNodeSetReconciler reconciles a StorageNodeSet object
type StorageNodeSetReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Namespace        string // operator namespace, used to look up the singleton ControlPlane CR
	TLSEnabled       bool
	TLSProvider      string
	TLSMutualEnabled bool
	Recorder         events.EventRecorder
}

type SNODEAPIResponse struct {
	UUID               string `json:"id"`
	Status             string `json:"status"`
	IP                 string `json:"mgmt_ip"`
	Health             bool   `json:"health_check"`
	Hostname           string `json:"hostname"`
	DevicesCount       int    `json:"device_count"`
	OnlineDevicesCount int    `json:"online_device_count"`
	CPU                int    `json:"cpu_spdk_count"`
	Memory             int64  `json:"spdk_mem"`
	Volumes            int    `json:"lvols"`
	RPC_PORT           int    `json:"rpc_port"`
	LVOL_PORT          int    `json:"lvol_subsys_port"`
	NVMF_PORT          int    `json:"nvmf_port"`
}

var (
	waitForNodeInfoReachableCheckFn    = checkNodeInfoReachable
	waitForNodeInfoReachableMaxRetries = 12
	waitForNodeInfoReachableRetryDelay = 10 * time.Second

	waitForNodeOnlineRetries         = 60
	waitForNodeOnlineWaitInterval    = 10 * time.Second
	waitForNodeOnlineActivationDelay = 120 * time.Second
	waitForNodeOnlineSleepFn         = time.Sleep

	syncNodeStatusInterval = 30 * time.Second

	spdkPodEventDelay = 20 * time.Second
)

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the StorageNodeSet object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *StorageNodeSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	snCR := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(ctx, req.NamespacedName, snCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterUUID, err := utils.ResolveClusterUUID(
		ctx,
		r.Client,
		snCR.Namespace,
		snCR.Spec.ClusterName,
	)

	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing",
			"cluster", snCR.Spec.ClusterName,
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	/* -------------------- Deletion -------------------- */
	if updated, err := r.handleDeletion(ctx, snCR); updated || err != nil {
		return ctrl.Result{}, err
	}

	/* -------------------- Finalizer -------------------- */
	if updated, err := r.ensureFinalizer(ctx, snCR); updated || err != nil {
		return ctrl.Result{}, err
	}

	apiClient := webapi.NewClient()

	if err := r.labelWorkerNodes(ctx, snCR); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileRBAC(ctx, snCR); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileService(ctx, snCR); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileSpdkProxyService(ctx, snCR); err != nil {
		return ctrl.Result{}, err
	}

	// Reconcile certificates before the DaemonSet so the TLS Secret is more
	// likely to exist when reconcileDaemonSet reads its resourceVersion to
	// stamp it as a pod-template annotation.
	if err := r.reconcileServingCertificates(ctx, snCR); err != nil {
		return ctrl.Result{}, err
	}

	// Reconcile per-node ConfigMap BEFORE the DaemonSet so that pods never
	// start without the ConfigMap already present.
	if err := r.reconcilePerNodeConfigMap(ctx, snCR); err != nil {
		log.Error(err, "failed to reconcile per-node ConfigMap")
	}

	if err := r.reconcileDaemonSet(ctx, snCR); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileEndpointSlice(ctx, snCR); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileSpdkProxyEndpointSlices(ctx, snCR); err != nil {
		return ctrl.Result{}, err
	}

	expectedPerHost := utils.ExpectedNodesPerHost(snCR)

	// Phase-1 bridge: create/sync/delete owned StorageNode CRs to match
	// spec.workerNodes × spec.socketsToUse. The StorageNodeReconciler owns
	// the per-node provisioning; this only manages CR lifecycle.
	if err := r.reconcileStorageNodeCRs(ctx, snCR); err != nil {
		log.Error(err, "failed to reconcile StorageNode CRs")
	}

	if res, err := r.reconcileWorkerNodes(ctx, snCR, clusterUUID, apiClient, expectedPerHost); err != nil || res.RequeueAfter > 0 {
		return res, err
	}

	if err := r.syncTrackedNodesStatus(ctx, apiClient, clusterUUID, snCR); err != nil {
		log.Error(err, "Failed to sync storage node status")
	}

	// Sync manually created StorageNode CRs (not in spec.workerNodes) into
	// StorageNodeSet.status.nodes[] so their status is visible in the fleet view.
	if err := r.syncManualStorageNodeStatus(ctx, snCR); err != nil {
		log.Error(err, "Failed to sync manual StorageNode status")
	}

	// On every reconcile, check whether the cluster is still unready and if
	// the activation conditions are now met. This catches cases where the
	// operator restarted after nodes came online but before activation fired,
	// or where the activation trigger was missed during concurrent reconciles.
	if clusterCR, err := utils.ResolveClusterCR(ctx, r.Client, snCR.Namespace, snCR.Spec.ClusterName); err == nil {
		if clusterCR.Status.Status == utils.ClusterStatusUnready {
			if activateErr := maybeActivateCluster(ctx, apiClient, clusterUUID, snCR, r); activateErr != nil {
				log.Info("Activation conditions not yet met", "reason", activateErr.Error())
			}
		}
	}

	hasTracked := false
	for _, n := range snCR.Status.Nodes {
		if n.UUID != "" {
			hasTracked = true
			break
		}
	}
	if !hasTracked {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: syncNodeStatusInterval}, nil
}

// reconcileWorkerNode handles provisioning and online-wait for a single worker node.
func (r *StorageNodeSetReconciler) reconcileWorkerNode(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	nodeName, clusterUUID string,
	apiClient *webapi.Client,
	expectedPerHost int,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Count status entries that have a UUID — these represent backend nodes
	// confirmed online at least once. Skip when all socket nodes are tracked.
	trackedCount := 0
	for _, n := range snCR.Status.Nodes {
		if n.Hostname == nodeName && n.UUID != "" {
			trackedCount++
		}
	}
	if trackedCount >= expectedPerHost {
		return ctrl.Result{}, nil
	}

	ip, err := getNodeInternalIP(ctx, r.Client, nodeName)
	if err != nil {
		log.Error(err, "failed to get internal IP", "node", nodeName)
		return ctrl.Result{RequeueAfter: time.Second * 10}, nil
	}

	// StorageNodeReconciler is the sole owner of provisioning. Only call
	// pollNodeOnline once ALL StorageNode CRs for this worker have their UUID set
	// (all nodes confirmed online). Until then requeue — calling pollNodeOnline
	// before all nodes are posted would time out waiting for expectedPerHost nodes.
	if r.storageNodeAlreadyPosted(ctx, snCR.Namespace, nodeName) {
		if r.allStorageNodesOnline(ctx, snCR.Namespace, nodeName, expectedPerHost) {
			return r.pollNodeOnline(ctx, apiClient, clusterUUID, ip, nodeName, expectedPerHost, snCR)
		}
		return ctrl.Result{RequeueAfter: waitForNodeOnlineWaitInterval}, nil
	}

	// StorageNodeReconciler is the sole owner of provisioning. If it hasn't
	// POSTed yet, requeue and wait — never POST from here.
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StorageNodeSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageNodeSet{}).
		Named("storagenodeset").
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.spdkProxyPodToStorageNodeSetRequests),
			builder.WithPredicates(
				predicate.NewPredicateFuncs(isSpdkProxyPod),
				predicate.Funcs{
					UpdateFunc: func(e event.UpdateEvent) bool {
						oldPod, ok := e.ObjectOld.(*corev1.Pod)
						if !ok {
							return true
						}
						newPod, ok := e.ObjectNew.(*corev1.Pod)
						if !ok {
							return true
						}
						return oldPod.Status.Phase != newPod.Status.Phase
					},
				},
			),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.tlsSecretToStorageNodeSetRequests),
			builder.WithPredicates(predicate.NewPredicateFuncs(isStorageNodeSetTLSSecret)),
		).
		Watches(
			&simplyblockv1alpha1.ControlPlane{},
			handler.EnqueueRequestsFromMapFunc(r.controlPlaneToStorageNodeSetRequests),
			builder.WithPredicates(predicate.NewPredicateFuncs(isSimplyblockControlPlane)),
		).
		Complete(r)
}

func isSpdkProxyPod(obj client.Object) bool {
	return obj.GetLabels()["role"] == utils.LabelSpdkProxyRole
}

func isStorageNodeSetTLSSecret(obj client.Object) bool {
	return obj.GetName() == utils.SecretNameStorageNodeSetAPITLS
}

func isSimplyblockControlPlane(obj client.Object) bool {
	return obj.GetName() == SingletonControlPlaneName
}

func (r *StorageNodeSetReconciler) controlPlaneToStorageNodeSetRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var snList simplyblockv1alpha1.StorageNodeSetList
	if err := r.List(ctx, &snList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(snList.Items))
	for _, sn := range snList.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: sn.Namespace, Name: sn.Name},
		})
	}
	return reqs
}

// tlsSecretToStorageNodeSetRequests enqueues every StorageNodeSet CR in the
// Secret's namespace when the storage-node-api TLS Secret changes. Coupled
// with the resourceVersion annotation stamped on the DaemonSet pod template,
// this drives a rolling restart whenever cert-manager (or OpenShift's
// service-ca) rotates the Secret.
func (r *StorageNodeSetReconciler) tlsSecretToStorageNodeSetRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var snList simplyblockv1alpha1.StorageNodeSetList
	if err := r.List(ctx, &snList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(snList.Items))
	for _, sn := range snList.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: sn.Namespace, Name: sn.Name},
		})
	}
	return reqs
}

// spdkProxyPodToStorageNodeSetRequests enqueues every StorageNodeSet CR in the Pod's
// namespace when a spdk-proxy pod changes. Pods are created by the backend, not
// by the operator, so there is no forward owner reference — fanning out within
// the namespace is the simplest correct mapping and cheap in practice (one CR
// per namespace is typical).
func (r *StorageNodeSetReconciler) spdkProxyPodToStorageNodeSetRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var snList simplyblockv1alpha1.StorageNodeSetList
	if err := r.List(ctx, &snList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(snList.Items))
	for _, sn := range snList.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: sn.Namespace, Name: sn.Name},
		})
	}
	return reqs
}

func (r *StorageNodeSetReconciler) handleDeletion(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (bool, error) {

	if snCR.DeletionTimestamp.IsZero() {
		return false, nil
	}

	if !controllerutil.ContainsFinalizer(snCR, utils.FinalizerStorageNodeSet) {
		return true, nil
	}

	controllerutil.RemoveFinalizer(snCR, utils.FinalizerStorageNodeSet)
	return true, r.Update(ctx, snCR)
}

func (r *StorageNodeSetReconciler) ensureFinalizer(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (bool, error) {

	if controllerutil.ContainsFinalizer(snCR, utils.FinalizerStorageNodeSet) {
		return false, nil
	}

	controllerutil.AddFinalizer(snCR, utils.FinalizerStorageNodeSet)
	return true, r.Update(ctx, snCR)
}

func (r *StorageNodeSetReconciler) labelWorkerNodes(ctx context.Context, sn *simplyblockv1alpha1.StorageNodeSet) error {
	// Collect all workers: spec.workerNodes plus any manually created StorageNode CRs
	// that reference this StorageNodeSet but are not in spec.workerNodes.
	workers := make(map[string]struct{}, len(sn.Spec.WorkerNodes))
	for _, w := range sn.Spec.WorkerNodes {
		workers[w] = struct{}{}
	}

	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(sn.Namespace),
		client.MatchingFields{"spec.storageNodeSetRef": sn.Name},
	); err == nil {
		for _, snCR := range snList.Items {
			workers[snCR.Spec.WorkerNode] = struct{}{}
		}
	}

	key := "io.simplyblock.node-type"
	value := "simplyblock-storage-plane-" + sn.Spec.ClusterName

	for nodeName := range workers {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			r.Recorder.Eventf(sn, nil, corev1.EventTypeWarning, "WorkerNodeNotFound", "WorkerNodeNotFound",
				"worker node %q: %v", nodeName, err)
			return err
		}

		if node.Labels == nil {
			node.Labels = map[string]string{}
		}

		if node.Labels[key] == value {
			continue
		}

		node.Labels[key] = value
		if err := r.Update(ctx, &node); err != nil {
			return err
		}
	}

	return nil
}

func (r *StorageNodeSetReconciler) reconcileDaemonSet(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) error {

	if snCR.Spec.ClusterImage == "" {
		cp := &simplyblockv1alpha1.ControlPlane{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: SingletonControlPlaneName}, cp); err != nil {
			return fmt.Errorf("clusterImage not set and ControlPlane %q not found: %w", SingletonControlPlaneName, err)
		}
		if cp.Spec.Image == "" {
			return fmt.Errorf("clusterImage not set and ControlPlane %q has no spec.image", SingletonControlPlaneName)
		}
		snCR = snCR.DeepCopy()
		snCR.Spec.ClusterImage = cp.Spec.Image
	}

	tlsSecretRV, err := r.getTLSSecretResourceVersion(ctx, snCR.Namespace)
	if err != nil {
		return err
	}

	ds := utils.BuildStorageNodeSetDaemonSet(snCR, r.TLSEnabled, r.TLSMutualEnabled, r.TLSProvider, tlsSecretRV)

	if err := controllerutil.SetControllerReference(snCR, ds, r.Scheme); err != nil {
		return err
	}

	var existing appsv1.DaemonSet
	err = r.Get(ctx, client.ObjectKeyFromObject(ds), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, ds)
	}
	if err != nil {
		return err
	}

	ds.ResourceVersion = existing.ResourceVersion
	return r.Update(ctx, ds)
}

// getTLSSecretResourceVersion returns the storage-node-api TLS Secret's
// metadata.resourceVersion, or "" if TLS is disabled or the Secret has not
// been provisioned yet. The value is stamped onto the DaemonSet's pod
// template so that cert rotations (where the Secret object changes but its
// name does not) trigger a rolling restart.
func (r *StorageNodeSetReconciler) getTLSSecretResourceVersion(
	ctx context.Context,
	namespace string,
) (string, error) {
	if !r.TLSEnabled {
		return "", nil
	}
	var sec corev1.Secret
	err := r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      utils.SecretNameStorageNodeSetAPITLS,
	}, &sec)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return sec.ResourceVersion, nil
}

func (r *StorageNodeSetReconciler) reconcileService(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	svc := utils.BuildStorageNodeSetService(snCR, r.TLSEnabled, r.TLSProvider)
	if err := controllerutil.SetControllerReference(snCR, svc, r.Scheme); err != nil {
		return fmt.Errorf("failed to set Service owner reference: %w", err)
	}

	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(svc), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}

	svc.ResourceVersion = existing.ResourceVersion
	svc.Spec.ClusterIP = existing.Spec.ClusterIP
	return r.Update(ctx, svc)
}

func (r *StorageNodeSetReconciler) reconcileServingCertificates(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	if !r.TLSEnabled || !utils.IsCertManagerTLSProvider(r.TLSProvider) {
		return nil
	}

	certificates := []struct {
		serviceName string
		secretName  string
	}{
		{
			serviceName: "simplyblock-storage-node-api",
			secretName:  utils.SecretNameStorageNodeSetAPITLS,
		},
		{
			serviceName: "simplyblock-spdk-proxy",
			secretName:  utils.SecretNameSpdkProxyTLS,
		},
	}

	for _, cert := range certificates {
		if err := r.reconcileServingCertificate(ctx, snCR, cert.serviceName, cert.secretName); err != nil {
			return err
		}
	}

	return nil
}

func (r *StorageNodeSetReconciler) reconcileServingCertificate(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	serviceName, secretName string,
) error {
	cert := utils.BuildServiceServingCertificate(snCR.Namespace, serviceName, secretName)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		desired := utils.BuildServiceServingCertificate(snCR.Namespace, serviceName, secretName)
		cert.Object["spec"] = desired.Object["spec"]
		return controllerutil.SetControllerReference(snCR, cert, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to apply serving Certificate for %s: %w", serviceName, err)
	}

	return nil
}

func (r *StorageNodeSetReconciler) reconcileEndpointSlice(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	log := logf.FromContext(ctx)

	// Start with workers from spec.workerNodes.
	nodeIPs := make(map[string]string)
	for _, nodeName := range snCR.Spec.WorkerNodes {
		ip, err := getNodeInternalIP(ctx, r.Client, nodeName)
		if err != nil {
			log.Error(err, "failed to get internal IP for EndpointSlice, skipping node", "node", nodeName)
			continue
		}
		nodeIPs[nodeName] = ip
	}

	// Also include workers from manually created StorageNode CRs so their
	// per-node DNS hostname resolves and checkNodeInfoReachable succeeds.
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(snCR.Namespace),
		client.MatchingFields{"spec.storageNodeSetRef": snCR.Name},
	); err == nil {
		for _, sn := range snList.Items {
			if _, ok := nodeIPs[sn.Spec.WorkerNode]; ok {
				continue // already covered
			}
			ip, err := getNodeInternalIP(ctx, r.Client, sn.Spec.WorkerNode)
			if err != nil {
				log.Error(err, "failed to get IP for manual StorageNode worker, skipping", "worker", sn.Spec.WorkerNode)
				continue
			}
			nodeIPs[sn.Spec.WorkerNode] = ip
		}
	}

	return r.applyStorageNodeSetEndpointSlice(ctx, snCR, nodeIPs)
}

// applyStorageNodeSetEndpointSlice creates or updates the storage-node-api
// EndpointSlice with the supplied nodeIPs map.
func (r *StorageNodeSetReconciler) applyStorageNodeSetEndpointSlice(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	nodeIPs map[string]string,
) error {
	eps := utils.BuildStorageNodeSetEndpointSlice(snCR, nodeIPs)
	if err := controllerutil.SetControllerReference(snCR, eps, r.Scheme); err != nil {
		return fmt.Errorf("failed to set EndpointSlice owner reference: %w", err)
	}

	var existing discoveryv1.EndpointSlice
	err := r.Get(ctx, client.ObjectKeyFromObject(eps), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, eps)
	}
	if err != nil {
		return err
	}

	eps.ResourceVersion = existing.ResourceVersion
	return r.Update(ctx, eps)
}

func (r *StorageNodeSetReconciler) reconcileSpdkProxyService(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	svc := utils.BuildSpdkProxyService(snCR, r.TLSEnabled, r.TLSProvider)
	if err := controllerutil.SetControllerReference(snCR, svc, r.Scheme); err != nil {
		return fmt.Errorf("failed to set spdk-proxy Service owner reference: %w", err)
	}

	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(svc), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}

	svc.ResourceVersion = existing.ResourceVersion
	svc.Spec.ClusterIP = existing.Spec.ClusterIP
	return r.Update(ctx, svc)
}

func (r *StorageNodeSetReconciler) reconcileSpdkProxyEndpointSlices(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	log := logf.FromContext(ctx)

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(snCR.Namespace),
		client.MatchingLabels{"role": utils.LabelSpdkProxyRole},
	); err != nil {
		return fmt.Errorf("failed to list spdk-proxy pods: %w", err)
	}

	byPort := map[int32][]utils.SpdkProxyEndpoint{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !isSpdkProxyPodReady(pod) {
			continue
		}
		rpcPort, ok := extractSpdkProxyRpcPort(pod)
		if !ok {
			log.Info("skipping spdk-proxy pod: unable to determine RPC_PORT", "pod", pod.Name)
			continue
		}
		byPort[rpcPort] = append(byPort[rpcPort], utils.SpdkProxyEndpoint{
			NodeName: pod.Spec.NodeName,
			PodIP:    pod.Status.PodIP,
			RpcPort:  rpcPort,
		})
	}

	for rpcPort, endpoints := range byPort {
		eps, err := utils.BuildSpdkProxyEndpointSlice(snCR, rpcPort, endpoints)
		if err != nil {
			return err
		}
		if err := controllerutil.SetControllerReference(snCR, eps, r.Scheme); err != nil {
			return fmt.Errorf("failed to set spdk-proxy EndpointSlice owner reference: %w", err)
		}

		var existing discoveryv1.EndpointSlice
		err = r.Get(ctx, client.ObjectKeyFromObject(eps), &existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, eps); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		eps.ResourceVersion = existing.ResourceVersion
		if err := r.Update(ctx, eps); err != nil {
			return err
		}
	}

	// Delete orphaned slices whose RPC_PORT no longer has any ready pod.
	var existingSlices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &existingSlices,
		client.InNamespace(snCR.Namespace),
		client.MatchingLabels{"kubernetes.io/service-name": "simplyblock-spdk-proxy"},
	); err != nil {
		return fmt.Errorf("failed to list existing spdk-proxy EndpointSlices: %w", err)
	}
	for i := range existingSlices.Items {
		slice := &existingSlices.Items[i]
		if !metav1.IsControlledBy(slice, snCR) {
			continue
		}
		keep := false
		for _, p := range slice.Ports {
			if p.Port != nil {
				if _, ok := byPort[*p.Port]; ok {
					keep = true
					break
				}
			}
		}
		if keep {
			continue
		}
		if err := r.Delete(ctx, slice); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete stale spdk-proxy EndpointSlice %s: %w", slice.Name, err)
		}
	}

	return nil
}

// workerIsInFlight returns true if a node-add POST has already been sent for
// nodeName and is still being tracked — either via PendingNodeAdds (primary)
// or the legacy UUID=="" placeholder (backward compatibility).
func workerIsInFlight(snCR *simplyblockv1alpha1.StorageNodeSet, nodeName string) bool {
	if _, ok := snCR.Status.PendingNodeAdds[nodeName]; ok {
		return true
	}
	for _, n := range snCR.Status.Nodes {
		if n.Hostname == nodeName && n.UUID == "" {
			return true
		}
	}
	return false
}

// recordSpdkPodEvents finds the worker's pending SPDK pod, fetches its most
// recent Kubernetes event, and surfaces it on the StorageNodeSet CR status so
// operators can see why a pod is stuck without running kubectl describe.
func (r *StorageNodeSetReconciler) recordSpdkPodEvents(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	nodeName string,
) {
	log := logf.FromContext(ctx)

	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(snCR.Namespace),
		client.MatchingLabels{"role": utils.LabelSpdkProxyRole},
	); err != nil {
		log.Error(err, "recordSpdkPodEvents: failed to list SPDK pods", "node", nodeName)
		return
	}

	var targetPod *corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		if pod.Spec.NodeName == nodeName ||
			pod.Spec.NodeSelector["kubernetes.io/hostname"] == nodeName {
			targetPod = pod
			break
		}
	}
	if targetPod == nil {
		return
	}

	var eventList corev1.EventList
	if err := r.List(ctx, &eventList, client.InNamespace(snCR.Namespace)); err != nil {
		log.Error(err, "recordSpdkPodEvents: failed to list events", "node", nodeName)
		return
	}

	var latest *corev1.Event
	for i := range eventList.Items {
		ev := &eventList.Items[i]
		if ev.InvolvedObject.Name != targetPod.Name {
			continue
		}
		if latest == nil || ev.LastTimestamp.After(latest.LastTimestamp.Time) {
			latest = ev
		}
	}
	if latest == nil {
		return
	}

	r.Recorder.Eventf(snCR, nil, corev1.EventTypeWarning, latest.Reason, latest.Reason,
		"worker %s: %s", nodeName, latest.Message)
	r.emitOnStorageNodeForWorker(ctx, snCR, nodeName, corev1.EventTypeWarning, latest.Reason, latest.Message)

	// Persist the flag so the recovery event is emitted correctly even if the
	// operator restarts before the node comes online.
	patch := client.MergeFrom(snCR.DeepCopy())
	if snCR.Status.SchedulingFailedWorkers == nil {
		snCR.Status.SchedulingFailedWorkers = make(map[string]bool)
	}
	snCR.Status.SchedulingFailedWorkers[nodeName] = true
	if err := r.Status().Patch(ctx, snCR, patch); err != nil {
		log.Error(err, "recordSpdkPodEvents: failed to persist scheduling failure flag", "node", nodeName)
	}
}

// reconcileWorkerNodes fans out the node-add loop across parallel (non-FDB) and
// sequential (FDB) workers, respecting MaxParallelNodeAdds.
// MaxParallelNodeAdds carries a +kubebuilder:default=1 marker so the API server
// always populates it before the CR is stored — it is safe to dereference directly.
func (r *StorageNodeSetReconciler) reconcileWorkerNodes(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	clusterUUID string,
	apiClient *webapi.Client,
	expectedPerHost int,
) (ctrl.Result, error) {
	fdbWorkers := r.fdbWorkerSet(ctx, snCR)

	var parallelWorkers, sequentialWorkers []string
	for _, nodeName := range snCR.Spec.WorkerNodes {
		if fdbWorkers[nodeName] {
			sequentialWorkers = append(sequentialWorkers, nodeName)
		} else {
			parallelWorkers = append(parallelWorkers, nodeName)
		}
	}

	maxParallel := int(*snCR.Spec.MaxParallelNodeAdds)

	inFlight := 0
	for _, nodeName := range parallelWorkers {
		if workerIsInFlight(snCR, nodeName) {
			inFlight++
		}
	}
	availableSlots := maxParallel - inFlight

	var parallelRequeueAfter time.Duration
	for _, nodeName := range parallelWorkers {
		alreadyInFlight := workerIsInFlight(snCR, nodeName)
		if !alreadyInFlight {
			if availableSlots <= 0 {
				if waitForNodeOnlineWaitInterval > parallelRequeueAfter {
					parallelRequeueAfter = waitForNodeOnlineWaitInterval
				}
				continue
			}
		}
		res, err := r.reconcileWorkerNode(ctx, snCR, nodeName, clusterUUID, apiClient, expectedPerHost)
		if err != nil {
			return ctrl.Result{}, err
		}
		// Only count the slot if the POST was genuinely sent (PendingNodeAdds
		// was set). A transient failure (e.g. checkNodeInfoReachable) clears
		// PendingNodeAdds immediately, so the slot should not be consumed.
		if !alreadyInFlight && workerIsInFlight(snCR, nodeName) {
			availableSlots--
		}
		if res.RequeueAfter > parallelRequeueAfter {
			parallelRequeueAfter = res.RequeueAfter
		}
	}

	for _, nodeName := range sequentialWorkers {
		res, err := r.reconcileWorkerNode(ctx, snCR, nodeName, clusterUUID, apiClient, expectedPerHost)
		if err != nil {
			return ctrl.Result{}, err
		}
		// Always return early for sequential (FDB) workers on any requeue.
		// If a concurrent reconcile's PendingNodeAdds persist failed (conflict),
		// workerIsInFlight would be false on the local snCR even though the
		// worker is effectively claimed — continuing the loop would process the
		// next FDB worker in parallel, violating the one-at-a-time guarantee.
		if res.RequeueAfter > 0 {
			return res, nil
		}
	}

	return ctrl.Result{RequeueAfter: parallelRequeueAfter}, nil
}

// reconcileRBAC ensures the ServiceAccount, ClusterRole, and ClusterRoleBinding
// required by the storage-node DaemonSet are present and up to date.
func (r *StorageNodeSetReconciler) reconcileRBAC(ctx context.Context, snCR *simplyblockv1alpha1.StorageNodeSet) error {
	sa := utils.BuildStorageNodeSetServiceAccount(snCR.Namespace)
	if err := controllerutil.SetControllerReference(snCR, sa, r.Scheme); err != nil {
		return fmt.Errorf("failed to set ServiceAccount owner reference: %w", err)
	}
	desiredSAOwnerRefs := sa.OwnerReferences
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.OwnerReferences = desiredSAOwnerRefs
		return nil
	}); err != nil {
		return fmt.Errorf("failed to apply ServiceAccount: %w", err)
	}

	cr := utils.BuildStorageNodeSetClusterRole(ptr.BoolFromOrFalse(snCR.Spec.OpenShiftCluster))
	desiredCRRules := cr.Rules
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cr, func() error {
		cr.Rules = desiredCRRules
		return nil
	}); err != nil {
		return fmt.Errorf("failed to apply ClusterRole: %w", err)
	}

	crb := utils.BuildStorageNodeSetClusterRoleBinding(snCR.Namespace)
	desiredCRBSubjects := crb.Subjects
	desiredCRBRoleRef := crb.RoleRef
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, crb, func() error {
		crb.Subjects = desiredCRBSubjects
		crb.RoleRef = desiredCRBRoleRef
		return nil
	}); err != nil {
		return fmt.Errorf("failed to apply ClusterRoleBinding: %w", err)
	}
	return nil
}

// fdbWorkerSet returns the set of worker node names (from snCR.Spec.WorkerNodes)
// that currently host at least one FDB pod. These workers must be added
// sequentially to avoid simultaneous reboots that reduce FDB fault tolerance.
func (r *StorageNodeSetReconciler) fdbWorkerSet(ctx context.Context, snCR *simplyblockv1alpha1.StorageNodeSet) map[string]bool {
	workerSet := make(map[string]bool, len(snCR.Spec.WorkerNodes))
	for _, w := range snCR.Spec.WorkerNodes {
		workerSet[w] = false
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(snCR.Namespace),
		client.HasLabels{utils.LabelFDBClusterName},
	); err != nil {
		return workerSet
	}

	fdbWorkers := make(map[string]bool)
	for _, pod := range podList.Items {
		if pod.Spec.NodeName != "" {
			if _, isWorker := workerSet[pod.Spec.NodeName]; isWorker {
				fdbWorkers[pod.Spec.NodeName] = true
			}
		}
	}
	return fdbWorkers
}

func isSpdkProxyPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	if pod.Spec.NodeName == "" || pod.Status.PodIP == "" {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return len(pod.Status.ContainerStatuses) > 0
}

// extractSpdkProxyRpcPort reads RPC_PORT from the spdk-proxy-container env; as
// a defensive fallback it parses the pod name pattern
// snode-spdk-pod-<RPC_PORT>-<CLUSTER_ID>.
func extractSpdkProxyRpcPort(pod *corev1.Pod) (int32, bool) {
	for _, c := range pod.Spec.Containers {
		if c.Name != "spdk-proxy-container" {
			continue
		}
		for _, e := range c.Env {
			if e.Name != "RPC_PORT" || e.Value == "" {
				continue
			}
			n, err := strconv.ParseInt(e.Value, 10, 32)
			if err != nil {
				return 0, false
			}
			return int32(n), true
		}
	}

	const prefix = "snode-spdk-pod-"
	if rest, ok := strings.CutPrefix(pod.Name, prefix); ok {
		if dash := strings.Index(rest, "-"); dash > 0 {
			if n, err := strconv.ParseInt(rest[:dash], 10, 32); err == nil {
				return int32(n), true
			}
		}
	}
	return 0, false
}

func getNodeInternalIP(ctx context.Context, c client.Client, nodeName string) (string, error) {
	var node corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address, nil
		}
	}

	return "", fmt.Errorf("node %s has no InternalIP", nodeName)
}

func checkNodeInfoReachable(ctx context.Context, nodeName, namespace string, tlsEnabled, tlsMutualEnabled bool) error {
	scheme := "http"
	httpClient := &http.Client{Timeout: 3 * time.Second}
	if tlsEnabled {
		scheme = "https"
		certPath, keyPath := "", ""
		if tlsMutualEnabled {
			certPath = tlsutil.ServiceClientCertificatePath
			keyPath = tlsutil.ServiceClientKeyPath
		}
		c, err := tlsutil.BuildStorageNodeSetAPIClient(namespace, tlsutil.ServiceCABundlePath, certPath, keyPath)
		if err != nil {
			return fmt.Errorf("build storage-node TLS client: %w", err)
		}
		httpClient = c
	}

	url := fmt.Sprintf("%s://%s/snode/info", scheme, utils.StorageNodeSetAPIAddress(nodeName, namespace))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("node info endpoint not reachable: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			fmt.Printf("warning: failed to close response body: %v\n", cerr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("node info endpoint returned %d", resp.StatusCode)
	}

	return nil
}

func waitForNodeInfoReachable(
	ctx context.Context,
	nodeName string,
	namespace string, //nolint:unparam
	tlsEnabled, tlsMutualEnabled bool, //nolint:unparam
) error {
	log := logf.FromContext(ctx)

	var lastErr error

	for i := 1; i <= waitForNodeInfoReachableMaxRetries; i++ {

		if err := waitForNodeInfoReachableCheckFn(ctx, nodeName, namespace, tlsEnabled, tlsMutualEnabled); err == nil {
			log.Info("Storage node API is reachable",
				"node", nodeName,
				"attempt", i,
			)
			return nil
		} else {
			lastErr = err
			log.V(1).Info("Storage node API not reachable yet, retrying",
				"node", nodeName,
				"attempt", i,
				"error", err.Error(),
			)
		}

		select {
		case <-time.After(waitForNodeInfoReachableRetryDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return fmt.Errorf(
		"storage node API not reachable after %d retries: %w",
		waitForNodeInfoReachableMaxRetries,
		lastErr,
	)
}

// pollNodeOnline performs a single non-blocking check of whether the node is
// online, returning RequeueAfter if it isn't yet. This replaces the old
// blocking waitForNodeOnline loop so the reconcile worker goroutine stays free.
func (r *StorageNodeSetReconciler) pollNodeOnline(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID, ip, nodeName string,
	expectedPerHost int,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)

	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	log.Info("SNODE LIST raw API response", "endpoint", endpoint, "status", status, "body", string(body))

	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Failed to get storage node statuses", "node", nodeName, "status", status, "response", string(body))
		return ctrl.Result{RequeueAfter: waitForNodeOnlineWaitInterval}, nil
	}

	if strings.TrimSpace(string(body)) == "[]" {
		log.Info("Storage node list is empty", "node", nodeName)
		return r.nodeOnlineRequeueOrTimeout(ctx, nodeName, ip, snCR)
	}

	var apiResp []SNODEAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to unmarshal storage node response for %s: %v", nodeName, err)
	}

	// Collect all backend nodes for this host IP that are online+healthy.
	// When socketsToUse is set the backend creates one node per socket, all
	// sharing the same mgmt IP and Hostname — we need all of them online.
	onlineForHost := make([]SNODEAPIResponse, 0, expectedPerHost)
	for _, res := range apiResp {
		if res.IP == ip && res.Status == utils.NodeStatusOnline && res.Health {
			onlineForHost = append(onlineForHost, res)
		}
	}

	if len(onlineForHost) < expectedPerHost {
		log.Info("Not all socket nodes online yet",
			"node", nodeName,
			"online", len(onlineForHost),
			"expected", expectedPerHost,
		)
		return r.nodeOnlineRequeueOrTimeout(ctx, nodeName, ip, snCR)
	}

	// All socket nodes are online — sync status and check cluster activation.
	if err := onAllSocketNodesOnline(ctx, apiClient, clusterUUID, snCR, nodeName, onlineForHost, r); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Storage node created successfully", "node", nodeName)
	return ctrl.Result{}, nil
}

// nodeOnlineRequeueOrTimeout returns RequeueAfter when the node is still
// within the allowed wait window, or marks it as timed-out and returns done.
func (r *StorageNodeSetReconciler) nodeOnlineRequeueOrTimeout(
	ctx context.Context,
	nodeName, ip string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	timeout := time.Duration(waitForNodeOnlineRetries) * waitForNodeOnlineWaitInterval

	// Read the post timestamp from the StorageNode CR (set by StorageNodeReconciler).
	// Fall back to PendingNodeAdds (legacy) and status.nodes[].PostedAt for
	// deployments that pre-date the StorageNodeReconciler.
	var postedAt *metav1.Time
	if t := r.storageNodePostedAt(ctx, snCR.Namespace, nodeName); t != nil {
		postedAt = t
	} else if t2, ok := snCR.Status.PendingNodeAdds[nodeName]; ok {
		postedAt = &t2
	} else {
		for i := range snCR.Status.Nodes {
			n := &snCR.Status.Nodes[i]
			if n.Hostname == nodeName && n.UUID == "" && n.PostedAt != nil {
				postedAt = n.PostedAt
				break
			}
		}
	}

	if postedAt != nil {
		if time.Since(postedAt.Time) <= timeout {
			if time.Since(postedAt.Time) >= spdkPodEventDelay {
				r.recordSpdkPodEvents(ctx, snCR, nodeName)
			}
			return ctrl.Result{RequeueAfter: waitForNodeOnlineWaitInterval}, nil
		}
	}

	// Timed out (or no post timestamp found — treat as timed-out).
	log.Error(nil, "Timeout waiting for node to become online", "node", nodeName)
	updated := false
	for i := range snCR.Status.Nodes {
		if snCR.Status.Nodes[i].Hostname == nodeName {
			snCR.Status.Nodes[i].Status = "timeout"
			snCR.Status.Nodes[i].MgmtIp = ip
			updated = true
		}
	}
	if !updated {
		snCR.Status.Nodes = append(snCR.Status.Nodes, simplyblockv1alpha1.NodeStatus{
			Hostname: nodeName,
			MgmtIp:   ip,
			Status:   "timeout",
		})
	}
	if err := r.Status().Update(ctx, snCR); err != nil {
		log.Error(err, "Failed to update node status after timeout", "node", nodeName)
	}
	return ctrl.Result{}, nil
}

// onAllSocketNodesOnline syncs the StorageNodeSet status entries for all online
// socket nodes and triggers cluster activation when conditions are met.
func onAllSocketNodesOnline(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	nodeName string,
	onlineForHost []SNODEAPIResponse,
	r *StorageNodeSetReconciler,
) error {
	log := logf.FromContext(ctx)

	patch := client.MergeFrom(snCR.DeepCopy())
	changed := false

	for _, res := range onlineForHost {
		updated := simplyblockv1alpha1.NodeStatus{
			Hostname: nodeName,
			UUID:     res.UUID,
			Health:   res.Health,
			Status:   res.Status,
			MgmtIp:   res.IP,
			Devices:  fmt.Sprintf("%d/%d", res.DevicesCount, res.OnlineDevicesCount),
			CPU:      ptr.To(int32(res.CPU)),
			Memory:   utils.HumanBytes(res.Memory, "iec"),
			Volumes:  ptr.To(int32(res.Volumes)),
			RpcPort:  ptr.To(int32(res.RPC_PORT)),
			LvolPort: ptr.To(int32(res.LVOL_PORT)),
			NvmfPort: ptr.To(int32(res.NVMF_PORT)),
		}

		// Try to find existing entry by UUID first, then fall back to the
		// placeholder entry (UUID=="") created after the POST.
		matched := false
		for i := range snCR.Status.Nodes {
			n := &snCR.Status.Nodes[i]
			if n.Hostname == nodeName && (n.UUID == res.UUID || n.UUID == "") {
				if !reflect.DeepEqual(*n, updated) {
					*n = updated
					changed = true
				}
				matched = true
				break
			}
		}
		if !matched {
			snCR.Status.Nodes = append(snCR.Status.Nodes, updated)
			changed = true
		}
	}

	// All socket nodes confirmed online — remove the pending marker so the
	// worker is no longer considered in-flight.
	if _, ok := snCR.Status.PendingNodeAdds[nodeName]; ok {
		delete(snCR.Status.PendingNodeAdds, nodeName)
		changed = true
	}
	// Emit a recovery event only if the worker previously had a scheduling
	// failure, then clear the flag.
	if snCR.Status.SchedulingFailedWorkers[nodeName] {
		r.Recorder.Eventf(snCR, nil, corev1.EventTypeNormal, "NodeOnline", "NodeOnline",
			"worker %s: SPDK pod is now online after previous scheduling failure", nodeName)
		r.emitOnStorageNodeForWorker(ctx, snCR, nodeName, corev1.EventTypeNormal, "NodeOnline",
			fmt.Sprintf("SPDK pod is now online after previous scheduling failure on %s", nodeName))
		delete(snCR.Status.SchedulingFailedWorkers, nodeName)
		changed = true
	}
	if changed {
		if err := r.Status().Patch(ctx, snCR, patch); err != nil {
			log.Error(err, "Failed to patch node status to online", "node", nodeName)
		}
	}

	log.Info("All socket nodes online", "node", nodeName, "count", len(onlineForHost))

	return maybeActivateCluster(ctx, apiClient, clusterUUID, snCR, r)
}

// syncTrackedNodesStatus refreshes all tracked (UUID != "") NodeStatus entries
// from the backend API. It is called on every completed reconcile pass to keep
// Health, Status, LvolPort and the other fields up-to-date after initial
// provisioning. PostedAt is preserved because it is a creation timestamp.
func (r *StorageNodeSetReconciler) syncTrackedNodesStatus(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	log := logf.FromContext(ctx)

	hasTracked := false
	for _, n := range snCR.Status.Nodes {
		if n.UUID != "" {
			hasTracked = true
			break
		}
	}
	if !hasTracked {
		return nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		return fmt.Errorf("sync: failed to list storage nodes: %w", err)
	}

	var apiResp []SNODEAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return fmt.Errorf("sync: failed to unmarshal storage node response: %w", err)
	}

	byUUID := make(map[string]SNODEAPIResponse, len(apiResp))
	for _, res := range apiResp {
		byUUID[res.UUID] = res
	}

	patch := client.MergeFrom(snCR.DeepCopy())
	changed := false

	for i := range snCR.Status.Nodes {
		n := &snCR.Status.Nodes[i]
		if n.UUID == "" {
			continue
		}
		res, ok := byUUID[n.UUID]
		if !ok {
			continue
		}
		updated := simplyblockv1alpha1.NodeStatus{
			Hostname: n.Hostname,
			UUID:     res.UUID,
			Health:   res.Health,
			Status:   res.Status,
			MgmtIp:   res.IP,
			Devices:  fmt.Sprintf("%d/%d", res.DevicesCount, res.OnlineDevicesCount),
			CPU:      ptr.To(int32(res.CPU)),
			Memory:   utils.HumanBytes(res.Memory, "iec"),
			Volumes:  ptr.To(int32(res.Volumes)),
			RpcPort:  ptr.To(int32(res.RPC_PORT)),
			LvolPort: ptr.To(int32(res.LVOL_PORT)),
			NvmfPort: ptr.To(int32(res.NVMF_PORT)),
			PostedAt: n.PostedAt,
			Uptime:   n.Uptime,
		}
		if !reflect.DeepEqual(*n, updated) {
			*n = updated
			changed = true
		}
	}

	if changed {
		if err := r.Status().Patch(ctx, snCR, patch); err != nil {
			log.Error(err, "Failed to patch storage node status during sync")
			return err
		}
		log.Info("Storage node status synced")
	}
	return nil
}

// maybeActivateCluster activates the cluster when online-node conditions are met.
func maybeActivateCluster(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	r *StorageNodeSetReconciler,
) error {
	log := logf.FromContext(ctx)

	clusterCR, err := utils.ResolveClusterCR(ctx, r.Client, snCR.Namespace, snCR.Spec.ClusterName)
	if err != nil {
		log.Info("Cluster not found yet for activation check")
		return fmt.Errorf("cluster not found yet")
	}

	if utils.ClusterAlreadyActive(clusterCR) {
		log.Info("Cluster already active, skipping activation")
		return nil
	}

	if utils.ClusterInExpansion(clusterCR) {
		log.Info("Cluster In expansion, skipping activation")
		return nil
	}

	onlineHealthy := utils.CountOnlineHealthyNodes(snCR.Status.Nodes)
	log.Info("Evaluating cluster activation conditions",
		"erasureCodingScheme", clusterCR.Status.ErasureCodingScheme,
		"onlineHealthy", onlineHealthy,
	)

	requiredEc, err := utils.RequiredNodesFromErasureCodingScheme(clusterCR.Status.ErasureCodingScheme)
	if err != nil {
		log.Error(err, "Invalid erasure coding scheme")
		return err
	}

	if utils.ShouldActivateCluster(requiredEc, onlineHealthy, snCR) {
		waitForNodeOnlineSleepFn(waitForNodeOnlineActivationDelay)
		log.Info("Activation conditions met — activating cluster")
		if err := utils.ActivateClusterAndWait(ctx, apiClient, clusterUUID); err != nil {
			log.Error(err, "Cluster activation did not complete")
			return err
		}
		log.Info("Cluster successfully activated")
	}

	return nil
}
