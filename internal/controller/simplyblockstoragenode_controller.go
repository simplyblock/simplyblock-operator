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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/tlsutil"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// StorageNodeReconciler reconciles a StorageNode object
type StorageNodeReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Namespace        string // operator namespace, used to look up the singleton ControlPlane CR
	TLSEnabled       bool
	TLSProvider      string
	TLSMutualEnabled bool
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

	performNodeActionPostTriggerDelay = 5 * time.Second
	performNodeActionSleepFn          = time.Sleep

	waitForActionCompletionRetries      = 50
	waitForActionCompletionWaitInterval = 5 * time.Second
	waitForActionCompletionSleepFn      = time.Sleep

	syncNodeStatusInterval = 30 * time.Second

	spdkPodEventDelay = 20 * time.Second
)

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/finalizers,verbs=update
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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the StorageNode object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *StorageNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	snCR := &simplyblockv1alpha1.StorageNode{}
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

	if snCR.Spec.Action != "" {
		return r.reconcileAction(ctx, snCR, clusterUUID)
	}

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

	if res, err := r.reconcileWorkerNodes(ctx, req, snCR, clusterUUID, apiClient, expectedPerHost); err != nil || res.RequeueAfter > 0 {
		return res, err
	}

	if err := r.syncTrackedNodesStatus(ctx, apiClient, clusterUUID, snCR); err != nil {
		log.Error(err, "Failed to sync storage node status")
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
func (r *StorageNodeReconciler) reconcileWorkerNode(
	ctx context.Context,
	req ctrl.Request,
	snCR *simplyblockv1alpha1.StorageNode,
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

	// PendingNodeAdds is the authoritative guard against duplicate POSTs.
	// It is a separate map field so patches to Status.Nodes by
	// syncTrackedNodesStatus can never inadvertently delete it.
	// The legacy UUID=="" placeholder check is kept as a fallback for
	// CRs that existed before PendingNodeAdds was introduced.
	_, isPending := snCR.Status.PendingNodeAdds[nodeName]
	if !isPending {
		for _, n := range snCR.Status.Nodes {
			if n.Hostname == nodeName && n.UUID == "" {
				isPending = true
				break
			}
		}
	}

	if !isPending {
		// Persist the pending marker BEFORE the POST so that every future
		// reconcile — including those triggered while sbcli is retrying
		// internally after a failure — sees the marker and skips the POST.
		patch := client.MergeFrom(snCR.DeepCopy())
		if snCR.Status.PendingNodeAdds == nil {
			snCR.Status.PendingNodeAdds = make(map[string]metav1.Time)
		}
		snCR.Status.PendingNodeAdds[nodeName] = metav1.Now()
		if err := r.Status().Patch(ctx, snCR, patch); err != nil {
			log.Error(err, "Failed to persist pending node add marker before POST, retrying", "node", nodeName)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		if res, err := r.postStorageNode(ctx, req, snCR, nodeName, ip, clusterUUID, apiClient); err != nil || res.RequeueAfter > 0 {
			// POST failed — clear the pending marker so the next reconcile
			// retries the POST rather than waiting on a node that was never created.
			clearPatch := client.MergeFrom(snCR.DeepCopy())
			delete(snCR.Status.PendingNodeAdds, nodeName)
			if patchErr := r.Status().Patch(ctx, snCR, clearPatch); patchErr != nil {
				log.Error(patchErr, "Failed to clear pending node add marker after POST failure", "node", nodeName)
			}
			return res, err
		}
	}

	return r.pollNodeOnline(ctx, apiClient, clusterUUID, ip, nodeName, expectedPerHost, snCR)
}

// postStorageNode calls the backend storage-node creation API and records the
// placeholder status entry.
func (r *StorageNodeReconciler) postStorageNode(
	ctx context.Context,
	req ctrl.Request,
	snCR *simplyblockv1alpha1.StorageNode,
	nodeName, ip, clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if err := checkNodeInfoReachable(ctx, nodeName, snCR.Namespace, r.TLSEnabled, r.TLSMutualEnabled); err != nil {
		log.Info("Storage node API not reachable yet, requeueing",
			"node", nodeName,
			"ip", ip,
			"error", err.Error(),
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	nodeAddress := utils.StorageNodeAPIAddress(nodeName, snCR.Namespace)
	params := utils.StorageNodeAddParams{
		NodeAddress:         nodeAddress,
		InterfaceName:       snCR.Spec.MgmtIfname,
		SPDKImage:           snCR.Spec.SpdkImage,
		SPDKProxyImage:      snCR.Spec.SpdkProxyImage,
		SPDKDebug:           false,
		IdDeviceByNQN:       false,
		DataNics:            snCR.Spec.DataIfname,
		Namespace:           snCR.Namespace,
		JMPercent:           journalManagerPercentPerDevice(snCR),
		Partitions:          utils.IntPtrOrDefault(snCR.Spec.Partitions, 1),
		IOBufSmallPoolCount: 0,
		IOBufLargePoolCount: 0,
		HaJMCount:           journalManagerCount(snCR),
		CRName:              snCR.Name,
		CRNameSpace:         snCR.Namespace,
		CRPlural:            "storagenodes",
		Format4K:            utils.BoolPtrOrFalse(snCR.Spec.ForceFormat4K),
		SpdkSystemMemory:    snCR.Spec.SpdkSystemMemory,
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes", clusterUUID)

	jsonParams, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		log.Error(err, "Failed to marshal params")
	} else {
		log.Info("Sending Storage Node Add Request",
			"endpoint", endpoint,
			"request_body", string(jsonParams),
		)
	}

	body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, params)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "StorageNode creation failed", "status", status, "response", string(body))
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}

	log.Info("SNODE API call",
		"endpoint", endpoint,
		"status", status,
		"response", string(body),
	)

	if err := r.Get(ctx, req.NamespacedName, snCR); err != nil {
		return ctrl.Result{}, err
	}

	ensureNodeStatus(snCR, nodeName, ip)

	if err := r.Status().Update(ctx, snCR); err != nil {
		log.Error(err, "Failed to update storage node status")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StorageNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageNode{}).
		Named("storagenode").
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.spdkProxyPodToStorageNodeRequests),
			builder.WithPredicates(predicate.NewPredicateFuncs(isSpdkProxyPod)),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.tlsSecretToStorageNodeRequests),
			builder.WithPredicates(predicate.NewPredicateFuncs(isStorageNodeTLSSecret)),
		).
		Watches(
			&simplyblockv1alpha1.ControlPlane{},
			handler.EnqueueRequestsFromMapFunc(r.controlPlaneToStorageNodeRequests),
			builder.WithPredicates(predicate.NewPredicateFuncs(isSimplyblockControlPlane)),
		).
		Complete(r)
}

func isSpdkProxyPod(obj client.Object) bool {
	return obj.GetLabels()["role"] == utils.LabelSpdkProxyRole
}

func isStorageNodeTLSSecret(obj client.Object) bool {
	return obj.GetName() == utils.SecretNameStorageNodeAPITLS
}

func isSimplyblockControlPlane(obj client.Object) bool {
	return obj.GetName() == SingletonControlPlaneName
}

func (r *StorageNodeReconciler) controlPlaneToStorageNodeRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var snList simplyblockv1alpha1.StorageNodeList
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

// tlsSecretToStorageNodeRequests enqueues every StorageNode CR in the
// Secret's namespace when the storage-node-api TLS Secret changes. Coupled
// with the resourceVersion annotation stamped on the DaemonSet pod template,
// this drives a rolling restart whenever cert-manager (or OpenShift's
// service-ca) rotates the Secret.
func (r *StorageNodeReconciler) tlsSecretToStorageNodeRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var snList simplyblockv1alpha1.StorageNodeList
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

// spdkProxyPodToStorageNodeRequests enqueues every StorageNode CR in the Pod's
// namespace when a spdk-proxy pod changes. Pods are created by the backend, not
// by the operator, so there is no forward owner reference — fanning out within
// the namespace is the simplest correct mapping and cheap in practice (one CR
// per namespace is typical).
func (r *StorageNodeReconciler) spdkProxyPodToStorageNodeRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var snList simplyblockv1alpha1.StorageNodeList
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

func (r *StorageNodeReconciler) handleDeletion(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
) (bool, error) {

	if snCR.DeletionTimestamp.IsZero() {
		return false, nil
	}

	if !controllerutil.ContainsFinalizer(snCR, utils.FinalizerStorageNode) {
		return true, nil
	}

	controllerutil.RemoveFinalizer(snCR, utils.FinalizerStorageNode)
	return true, r.Update(ctx, snCR)
}

func (r *StorageNodeReconciler) ensureFinalizer(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
) (bool, error) {

	if controllerutil.ContainsFinalizer(snCR, utils.FinalizerStorageNode) {
		return false, nil
	}

	controllerutil.AddFinalizer(snCR, utils.FinalizerStorageNode)
	return true, r.Update(ctx, snCR)
}

func (r *StorageNodeReconciler) labelWorkerNodes(ctx context.Context, sn *simplyblockv1alpha1.StorageNode) error {
	for _, nodeName := range sn.Spec.WorkerNodes {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			return err
		}

		if node.Labels == nil {
			node.Labels = map[string]string{}
		}

		key := "io.simplyblock.node-type"
		value := "simplyblock-storage-plane-" + sn.Spec.ClusterName

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

func (r *StorageNodeReconciler) labelWorkerNode(ctx context.Context, sn *simplyblockv1alpha1.StorageNode) error {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: sn.Spec.WorkerNode}, &node); err != nil {
		return err
	}

	if node.Labels == nil {
		node.Labels = map[string]string{}
	}

	key := "io.simplyblock.node-type"
	value := "simplyblock-storage-plane-" + sn.Spec.ClusterName

	node.Labels[key] = value
	if err := r.Update(ctx, &node); err != nil {
		return err
	}

	return nil
}

func (r *StorageNodeReconciler) reconcileDaemonSet(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
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

	ds := utils.BuildStorageNodeDaemonSet(snCR, r.TLSEnabled, r.TLSMutualEnabled, r.TLSProvider, tlsSecretRV)

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
func (r *StorageNodeReconciler) getTLSSecretResourceVersion(
	ctx context.Context,
	namespace string,
) (string, error) {
	if !r.TLSEnabled {
		return "", nil
	}
	var sec corev1.Secret
	err := r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      utils.SecretNameStorageNodeAPITLS,
	}, &sec)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return sec.ResourceVersion, nil
}

func (r *StorageNodeReconciler) reconcileService(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
) error {
	svc := utils.BuildStorageNodeService(snCR, r.TLSEnabled, r.TLSProvider)
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

func (r *StorageNodeReconciler) reconcileServingCertificates(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
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
			secretName:  utils.SecretNameStorageNodeAPITLS,
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

func (r *StorageNodeReconciler) reconcileServingCertificate(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
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

func (r *StorageNodeReconciler) reconcileEndpointSlice(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
) error {
	log := logf.FromContext(ctx)

	nodeIPs := make(map[string]string)
	for _, nodeName := range snCR.Spec.WorkerNodes {
		ip, err := getNodeInternalIP(ctx, r.Client, nodeName)
		if err != nil {
			log.Error(err, "failed to get internal IP for EndpointSlice, skipping node", "node", nodeName)
			continue
		}
		nodeIPs[nodeName] = ip
	}

	eps := utils.BuildStorageNodeEndpointSlice(snCR, nodeIPs)
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

func (r *StorageNodeReconciler) reconcileSpdkProxyService(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
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

func (r *StorageNodeReconciler) reconcileSpdkProxyEndpointSlices(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
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
func workerIsInFlight(snCR *simplyblockv1alpha1.StorageNode, nodeName string) bool {
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
// recent Kubernetes event, and surfaces it on the StorageNode CR status so
// operators can see why a pod is stuck without running kubectl describe.
func (r *StorageNodeReconciler) recordSpdkPodEvents(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
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
		if pod.Spec.NodeName == nodeName && pod.Status.Phase == corev1.PodPending {
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

	patch := client.MergeFrom(snCR.DeepCopy())
	if snCR.Status.NodePodEvents == nil {
		snCR.Status.NodePodEvents = make(map[string]simplyblockv1alpha1.NodePodEvent)
	}
	snCR.Status.NodePodEvents[nodeName] = simplyblockv1alpha1.NodePodEvent{
		Reason:     latest.Reason,
		Message:    latest.Message,
		ObservedAt: metav1.Now(),
	}
	if err := r.Status().Patch(ctx, snCR, patch); err != nil {
		log.Error(err, "recordSpdkPodEvents: failed to patch pod event status", "node", nodeName)
	}
}

// reconcileWorkerNodes fans out the node-add loop across parallel (non-FDB) and
// sequential (FDB) workers, respecting MaxParallelNodeAdds.
// MaxParallelNodeAdds carries a +kubebuilder:default=1 marker so the API server
// always populates it before the CR is stored — it is safe to dereference directly.
func (r *StorageNodeReconciler) reconcileWorkerNodes(
	ctx context.Context,
	req ctrl.Request,
	snCR *simplyblockv1alpha1.StorageNode,
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
			availableSlots--
		}
		res, err := r.reconcileWorkerNode(ctx, req, snCR, nodeName, clusterUUID, apiClient, expectedPerHost)
		if err != nil {
			return ctrl.Result{}, err
		}
		if res.RequeueAfter > parallelRequeueAfter {
			parallelRequeueAfter = res.RequeueAfter
		}
	}

	for _, nodeName := range sequentialWorkers {
		res, err := r.reconcileWorkerNode(ctx, req, snCR, nodeName, clusterUUID, apiClient, expectedPerHost)
		if err != nil {
			return ctrl.Result{}, err
		}
		if res.RequeueAfter > 0 {
			return res, nil
		}
	}

	return ctrl.Result{RequeueAfter: parallelRequeueAfter}, nil
}

// reconcileRBAC ensures the ServiceAccount, ClusterRole, and ClusterRoleBinding
// required by the storage-node DaemonSet are present and up to date.
func (r *StorageNodeReconciler) reconcileRBAC(ctx context.Context, snCR *simplyblockv1alpha1.StorageNode) error {
	sa := utils.BuildStorageNodeServiceAccount(snCR.Namespace)
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

	cr := utils.BuildStorageNodeClusterRole(utils.BoolPtrOrFalse(snCR.Spec.OpenShiftCluster))
	desiredCRRules := cr.Rules
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cr, func() error {
		cr.Rules = desiredCRRules
		return nil
	}); err != nil {
		return fmt.Errorf("failed to apply ClusterRole: %w", err)
	}

	crb := utils.BuildStorageNodeClusterRoleBinding(snCR.Namespace)
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
func (r *StorageNodeReconciler) fdbWorkerSet(ctx context.Context, snCR *simplyblockv1alpha1.StorageNode) map[string]bool {
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
			n, err := strconv.Atoi(e.Value)
			if err != nil {
				return 0, false
			}
			return int32(n), true
		}
	}

	const prefix = "snode-spdk-pod-"
	if rest, ok := strings.CutPrefix(pod.Name, prefix); ok {
		if dash := strings.Index(rest, "-"); dash > 0 {
			if n, err := strconv.Atoi(rest[:dash]); err == nil {
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

func ensureNodeStatus(
	snCR *simplyblockv1alpha1.StorageNode,
	nodeName, ip string,
) *simplyblockv1alpha1.NodeStatus {

	for i := range snCR.Status.Nodes {
		if snCR.Status.Nodes[i].Hostname == nodeName {
			return &snCR.Status.Nodes[i]
		}
	}

	now := metav1.Now()
	snCR.Status.Nodes = append(snCR.Status.Nodes, simplyblockv1alpha1.NodeStatus{
		Hostname: nodeName,
		MgmtIp:   ip,
		Status:   "in_creation",
		PostedAt: &now,
	})

	return &snCR.Status.Nodes[len(snCR.Status.Nodes)-1]
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
		c, err := tlsutil.BuildStorageNodeAPIClient(namespace, tlsutil.ServiceCABundlePath, certPath, keyPath)
		if err != nil {
			return fmt.Errorf("build storage-node TLS client: %w", err)
		}
		httpClient = c
	}

	url := fmt.Sprintf("%s://%s/snode/info", scheme, utils.StorageNodeAPIAddress(nodeName, namespace))

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
	namespace string,
	tlsEnabled, tlsMutualEnabled bool,
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
			log.Info("Storage node API not reachable yet, retrying",
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
func (r *StorageNodeReconciler) pollNodeOnline(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID, ip, nodeName string,
	expectedPerHost int,
	snCR *simplyblockv1alpha1.StorageNode,
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
func (r *StorageNodeReconciler) nodeOnlineRequeueOrTimeout(
	ctx context.Context,
	nodeName, ip string,
	snCR *simplyblockv1alpha1.StorageNode,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	timeout := time.Duration(waitForNodeOnlineRetries) * waitForNodeOnlineWaitInterval

	// PendingNodeAdds is the primary source for the post timestamp.
	// Fall back to the legacy UUID=="" PostedAt for backward compatibility.
	if postedAt, ok := snCR.Status.PendingNodeAdds[nodeName]; ok {
		if time.Since(postedAt.Time) <= timeout {
			if time.Since(postedAt.Time) >= spdkPodEventDelay {
				r.recordSpdkPodEvents(ctx, snCR, nodeName)
			}
			return ctrl.Result{RequeueAfter: waitForNodeOnlineWaitInterval}, nil
		}
	} else {
		for i := range snCR.Status.Nodes {
			n := &snCR.Status.Nodes[i]
			if n.Hostname == nodeName && n.UUID == "" && n.PostedAt != nil {
				if time.Since(n.PostedAt.Time) <= timeout {
					return ctrl.Result{RequeueAfter: waitForNodeOnlineWaitInterval}, nil
				}
			}
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

// onAllSocketNodesOnline syncs the StorageNode status entries for all online
// socket nodes and triggers cluster activation when conditions are met.
func onAllSocketNodesOnline(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNode,
	nodeName string,
	onlineForHost []SNODEAPIResponse,
	r *StorageNodeReconciler,
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
			CPU:      utils.IntToInt32Ptr(res.CPU),
			Memory:   utils.HumanBytes(res.Memory, "iec"),
			Volumes:  utils.IntToInt32Ptr(res.Volumes),
			RpcPort:  utils.IntToInt32Ptr(res.RPC_PORT),
			LvolPort: utils.IntToInt32Ptr(res.LVOL_PORT),
			NvmfPort: utils.IntToInt32Ptr(res.NVMF_PORT),
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

	// All socket nodes confirmed online — remove the pending marker and any
	// recorded pod events so the worker is no longer considered in-flight.
	if _, ok := snCR.Status.PendingNodeAdds[nodeName]; ok {
		delete(snCR.Status.PendingNodeAdds, nodeName)
		changed = true
	}
	if _, ok := snCR.Status.NodePodEvents[nodeName]; ok {
		delete(snCR.Status.NodePodEvents, nodeName)
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
func (r *StorageNodeReconciler) syncTrackedNodesStatus(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNode,
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
			CPU:      utils.IntToInt32Ptr(res.CPU),
			Memory:   utils.HumanBytes(res.Memory, "iec"),
			Volumes:  utils.IntToInt32Ptr(res.Volumes),
			RpcPort:  utils.IntToInt32Ptr(res.RPC_PORT),
			LvolPort: utils.IntToInt32Ptr(res.LVOL_PORT),
			NvmfPort: utils.IntToInt32Ptr(res.NVMF_PORT),
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
	snCR *simplyblockv1alpha1.StorageNode,
	r *StorageNodeReconciler,
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

func journalManagerPercentPerDevice(
	snCR *simplyblockv1alpha1.StorageNode,
) int {
	if snCR.Spec.JournalManagerSpec == nil {
		return 3
	}
	return utils.IntPtrOrDefault(snCR.Spec.JournalManagerSpec.PercentPerDevice, 3)
}

func journalManagerCount(
	snCR *simplyblockv1alpha1.StorageNode,
) int {
	if snCR.Spec.JournalManagerSpec == nil {
		return 3
	}
	return utils.IntPtrOrDefault(snCR.Spec.JournalManagerSpec.Count, 3)
}

func (r *StorageNodeReconciler) reconcileAction(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
) (ctrl.Result, error) {

	apiClient := webapi.NewClient()

	if err := r.handleNodeAction(
		ctx,
		apiClient,
		snCR,
		clusterUUID,
	); err != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *StorageNodeReconciler) handleNodeAction(
	ctx context.Context,
	apiClient *webapi.Client,
	snCR *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
) error {
	log := logf.FromContext(ctx)

	// Skip if already successful
	if snCR.Status.ActionStatus != nil &&
		snCR.Status.ActionStatus.Action == snCR.Spec.Action &&
		snCR.Status.ActionStatus.NodeUUID == snCR.Spec.NodeUUID &&
		snCR.Status.ActionStatus.State == utils.ActionStateSuccess {
		log.Info("Action already completed successfully, skipping",
			"action", snCR.Spec.Action,
			"nodeUUID", snCR.Spec.NodeUUID,
		)
		return nil
	}

	snCR.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
		Action:    snCR.Spec.Action,
		NodeUUID:  snCR.Spec.NodeUUID,
		State:     utils.ActionStateRunning,
		UpdatedAt: metav1.Now(),
	}
	if err := r.Status().Update(ctx, snCR); err != nil {
		log.Error(err, "Failed to set action status to running")
		return err
	}

	if err := r.performNodeAction(ctx, apiClient, clusterUUID, snCR); err != nil {
		log.Error(err, "Action failed", "action", snCR.Spec.Action, "nodeUUID", snCR.Spec.NodeUUID)
		snCR.Status.ActionStatus.State = utils.ActionStateFailed
		snCR.Status.ActionStatus.Message = err.Error()
		snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
		_ = r.Status().Update(ctx, snCR)
		return err
	}

	snCR.Status.ActionStatus.State = utils.ActionStateSuccess
	snCR.Status.ActionStatus.Message = "Action executed successfully"
	snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
	if err := r.Status().Update(ctx, snCR); err != nil {
		log.Error(err, "Failed to update action status")
		return err
	}

	log.Info("Action completed successfully", "action", snCR.Spec.Action, "nodeUUID", snCR.Spec.NodeUUID)
	return nil
}

func (r *StorageNodeReconciler) performNodeAction(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNode,
) error {

	log := logf.FromContext(ctx)

	var (
		endpoint string
		method   = http.MethodPost
		body     any
	)

	switch snCR.Spec.Action {

	case "restart":
		payload := map[string]any{
			"force":           nodeActionForce(snCR, true),
			"reattach_volume": utils.BoolPtrOrFalse(snCR.Spec.ReattachVolume),
		}

		if snCR.Spec.WorkerNode != "" {
			if err := r.labelWorkerNode(ctx, snCR); err != nil {
				return fmt.Errorf("failed to label worker node %s: %w", snCR.Spec.WorkerNode, err)
			}

			if err := waitForNodeInfoReachable(ctx, snCR.Spec.WorkerNode, snCR.Namespace, r.TLSEnabled, r.TLSMutualEnabled); err != nil {
				log.Error(err, "node never became reachable")
				return err
			}

			body = map[string]any{
				"force":           nodeActionForce(snCR, true),
				"reattach_volume": utils.BoolPtrOrFalse(snCR.Spec.ReattachVolume),
				"node_address":    utils.StorageNodeAPIAddress(snCR.Spec.WorkerNode, snCR.Namespace),
			}
		} else {
			body = payload
		}

		endpoint = fmt.Sprintf(
			"/api/v2/clusters/%s/storage-nodes/%s/restart",
			clusterUUID,
			snCR.Spec.NodeUUID,
		)

	case "remove":
		method = http.MethodDelete
		body = nil
		endpoint = fmt.Sprintf(
			"/api/v2/clusters/%s/storage-nodes/%s?force_remove=%t",
			clusterUUID,
			snCR.Spec.NodeUUID,
			nodeActionForce(snCR, true),
		)

	default:
		body = nil
		endpoint = fmt.Sprintf(
			"/api/v2/clusters/%s/storage-nodes/%s/%s",
			clusterUUID,
			snCR.Spec.NodeUUID,
			snCR.Spec.Action,
		)
	}

	respBody, status, err := apiClient.Do(ctx, method, endpoint, body)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Node action API call failed", "action", snCR.Spec.Action, "nodeUUID", snCR.Spec.NodeUUID, "status", status, "response", string(respBody))
		return fmt.Errorf("action API failed: status=%d err=%v", status, err)
	}

	log.Info(
		"Node action triggered",
		"nodeUUID", snCR.Spec.NodeUUID,
		"action", snCR.Spec.Action,
		"response", string(respBody),
	)

	performNodeActionSleepFn(performNodeActionPostTriggerDelay)

	if err := r.waitForActionCompletion(
		ctx,
		apiClient,
		clusterUUID,
		snCR.Spec.NodeUUID,
		snCR.Spec.Action,
	); err != nil {
		return fmt.Errorf(
			"node did not reach expected state after action %s: %w",
			snCR.Spec.Action,
			err,
		)
	}

	log.Info(
		"Node reached expected state",
		"nodeUUID", snCR.Spec.NodeUUID,
		"action", snCR.Spec.Action,
	)

	return nil
}

func nodeActionForce(snCR *simplyblockv1alpha1.StorageNode, defaultValue bool) bool {
	if snCR.Spec.Force == nil {
		return defaultValue
	}
	return *snCR.Spec.Force
}

func (r *StorageNodeReconciler) waitForActionCompletion(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	nodeUUID string,
	action string,
) error {

	log := logf.FromContext(ctx)

	expectedStatus := map[string]string{
		"suspend":  "suspended",
		"resume":   "online",
		"shutdown": "offline",
		"restart":  "online",
		"remove":   "removed",
	}

	targetStatus, ok := expectedStatus[action]
	if !ok {
		return fmt.Errorf("unknown action: %s", action)
	}

	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-nodes/%s",
		clusterUUID,
		nodeUUID,
	)

	for i := 0; i < waitForActionCompletionRetries; i++ {
		body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)

		if action == "remove" && status == http.StatusNotFound {
			log.Info(
				"Node successfully removed (404 returned)",
				"nodeUUID", nodeUUID,
			)
			return nil
		}

		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d", status)
			}
			log.Error(
				err,
				"Failed to get node status",
				"nodeUUID", nodeUUID,
				"status", status,
				"response", string(body),
			)
			waitForActionCompletionSleepFn(waitForActionCompletionWaitInterval)
			continue
		}

		var resp utils.NodeStatusResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			log.Error(err, "Failed to parse node status response", "body", string(body))
			waitForActionCompletionSleepFn(waitForActionCompletionWaitInterval)
			continue
		}

		if resp.Status == targetStatus {
			log.Info(
				"Node reached expected status",
				"nodeUUID", nodeUUID,
				"status", resp.Status,
			)
			return nil
		}

		waitForActionCompletionSleepFn(waitForActionCompletionWaitInterval)
	}

	return fmt.Errorf(
		"node %s did not reach expected status %q after action %q",
		nodeUUID,
		targetStatus,
		action,
	)
}
