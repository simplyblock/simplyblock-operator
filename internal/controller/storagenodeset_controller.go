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
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// StorageNodeSetReconciler reconciles a StorageNodeSet object
type StorageNodeSetReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Namespace        string
	TLSEnabled       bool
	TLSProvider      string
	TLSMutualEnabled bool
}

// snSetToSN builds a lightweight StorageNode adapter from a StorageNodeSet so
// that existing utility functions (which accept *StorageNode) can be reused
// without modification. The adapter is used only for reads; status writes are
// always performed on the original StorageNodeSet object.
func snSetToSN(sns *simplyblockv1alpha1.StorageNodeSet) *simplyblockv1alpha1.StorageNode {
	sn := &simplyblockv1alpha1.StorageNode{}
	sn.ObjectMeta = sns.ObjectMeta
	sn.Spec = sns.Spec     // StorageNodeSetSpec = StorageNodeSpec
	sn.Status = sns.Status // StorageNodeSetStatus = StorageNodeStatus
	return sn
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets/finalizers,verbs=update

func (r *StorageNodeSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	snsCR := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(ctx, req.NamespacedName, snsCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterUUID, err := utils.ResolveClusterUUID(
		ctx,
		r.Client,
		snsCR.Namespace,
		snsCR.Spec.ClusterName,
	)
	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing", "cluster", snsCR.Spec.ClusterName)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	_, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, snsCR.Namespace, snsCR.Spec.ClusterName)
	if err != nil {
		log.Error(err, "Failed to get cluster auth")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if updated, err := r.handleDeletion(ctx, snsCR); updated || err != nil {
		return ctrl.Result{}, err
	}

	if updated, err := r.ensureFinalizer(ctx, snsCR); updated || err != nil {
		return ctrl.Result{}, err
	}

	apiClient := webapi.NewClient()

	if snsCR.Spec.Action != "" {
		return r.reconcileAction(ctx, snsCR, clusterUUID, clusterSecret)
	}

	if err := r.labelWorkerNodes(ctx, snsCR); err != nil {
		return ctrl.Result{}, err
	}

	sa := utils.BuildStorageNodeServiceAccount(snsCR.Namespace)
	if err := controllerutil.SetControllerReference(snsCR, sa, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set ServiceAccount owner reference: %w", err)
	}
	desiredSAOwnerRefs := sa.OwnerReferences
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.OwnerReferences = desiredSAOwnerRefs
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply ServiceAccount: %w", err)
	}

	cr := utils.BuildStorageNodeClusterRole(utils.BoolPtrOrFalse(snsCR.Spec.OpenShiftCluster))
	desiredCRRules := cr.Rules
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cr, func() error {
		cr.Rules = desiredCRRules
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply ClusterRole: %w", err)
	}

	crb := utils.BuildStorageNodeClusterRoleBinding(snsCR.Namespace)
	desiredCRBSubjects := crb.Subjects
	desiredCRBRoleRef := crb.RoleRef
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, crb, func() error {
		crb.Subjects = desiredCRBSubjects
		crb.RoleRef = desiredCRBRoleRef
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply ClusterRoleBinding: %w", err)
	}

	if err := r.reconcileService(ctx, snsCR); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileSpdkProxyService(ctx, snsCR); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileServingCertificates(ctx, snsCR); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileDaemonSet(ctx, snsCR); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileEndpointSlice(ctx, snsCR); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileSpdkProxyEndpointSlices(ctx, snsCR); err != nil {
		return ctrl.Result{}, err
	}

	expectedPerHost := utils.ExpectedNodesPerHost(snSetToSN(snsCR))

	for _, nodeName := range snsCR.Spec.WorkerNodes {
		res, err := r.reconcileWorkerNode(ctx, req, snsCR, nodeName, clusterUUID, clusterSecret, apiClient, expectedPerHost)
		if err != nil || res.RequeueAfter > 0 {
			return res, err
		}
	}

	if err := r.syncTrackedNodesStatus(ctx, apiClient, clusterSecret, clusterUUID, snsCR); err != nil {
		log.Error(err, "Failed to sync storage node set status")
	}

	hasTracked := false
	for _, n := range snsCR.Status.Nodes {
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

func (r *StorageNodeSetReconciler) reconcileWorkerNode(
	ctx context.Context,
	req ctrl.Request,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
	nodeName, clusterUUID, clusterSecret string,
	apiClient *webapi.Client,
	expectedPerHost int,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	trackedCount := 0
	for _, n := range snsCR.Status.Nodes {
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

	hasPlaceholder := false
	for _, n := range snsCR.Status.Nodes {
		if n.Hostname == nodeName && n.UUID == "" {
			hasPlaceholder = true
			break
		}
	}

	if !hasPlaceholder {
		if res, err := r.postStorageNodeSet(ctx, req, snsCR, nodeName, ip, clusterUUID, clusterSecret, apiClient); err != nil || res.RequeueAfter > 0 {
			return res, err
		}
	}

	return r.pollNodeSetOnline(ctx, apiClient, clusterSecret, clusterUUID, ip, nodeName, expectedPerHost, snsCR)
}

func (r *StorageNodeSetReconciler) postStorageNodeSet(
	ctx context.Context,
	req ctrl.Request,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
	nodeName, ip, clusterUUID, clusterSecret string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if err := checkNodeInfoReachable(ctx, nodeName, snsCR.Namespace, r.TLSEnabled, r.TLSMutualEnabled); err != nil {
		log.Info("Storage node API not reachable yet, requeueing",
			"node", nodeName,
			"ip", ip,
			"error", err.Error(),
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	nodeAddress := utils.StorageNodeAPIAddress(nodeName, snsCR.Namespace)
	params := utils.StorageNodeAddParams{
		NodeAddress:         nodeAddress,
		InterfaceName:       snsCR.Spec.MgmtIfname,
		SPDKImage:           snsCR.Spec.SpdkImage,
		SPDKProxyImage:      snsCR.Spec.SpdkProxyImage,
		SPDKDebug:           false,
		IdDeviceByNQN:       false,
		DataNics:            snsCR.Spec.DataIfname,
		Namespace:           snsCR.Namespace,
		JMPercent:           journalManagerPercentPerDevice(snSetToSN(snsCR)),
		Partitions:          utils.IntPtrOrDefault(snsCR.Spec.Partitions, 1),
		IOBufSmallPoolCount: 0,
		IOBufLargePoolCount: 0,
		HaJMCount:           journalManagerCount(snSetToSN(snsCR)),
		CRName:              snsCR.Name,
		CRNameSpace:         snsCR.Namespace,
		CRPlural:            "storagenodesets",
		Format4K:            utils.BoolPtrOrFalse(snsCR.Spec.ForceFormat4K),
		SpdkSystemMemory:    snsCR.Spec.SpdkSystemMemory,
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes", clusterUUID)

	jsonParams, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		log.Error(err, "Failed to marshal params")
	} else {
		log.Info("Sending Storage Node Set Add Request",
			"endpoint", endpoint,
			"request_body", string(jsonParams),
		)
	}

	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, params)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "StorageNodeSet creation failed", "status", status, "response", string(body))
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}

	log.Info("SNODE API call",
		"endpoint", endpoint,
		"status", status,
		"response", string(body),
	)

	if err := r.Get(ctx, req.NamespacedName, snsCR); err != nil {
		return ctrl.Result{}, err
	}

	ensureNodeSetStatus(snsCR, nodeName, ip)

	if err := r.Status().Update(ctx, snsCR); err != nil {
		log.Error(err, "Failed to update storage node set status")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StorageNodeSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageNodeSet{}).
		Named("storagenodeset").
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.spdkProxyPodToStorageNodeSetRequests),
			builder.WithPredicates(predicate.NewPredicateFuncs(isSpdkProxyPod)),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.tlsSecretToStorageNodeSetRequests),
			builder.WithPredicates(predicate.NewPredicateFuncs(isStorageNodeTLSSecret)),
		).
		Watches(
			&simplyblockv1alpha1.ControlPlane{},
			handler.EnqueueRequestsFromMapFunc(r.controlPlaneToStorageNodeSetRequests),
			builder.WithPredicates(predicate.NewPredicateFuncs(isSimplyblockControlPlane)),
		).
		Complete(r)
}

func (r *StorageNodeSetReconciler) controlPlaneToStorageNodeSetRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var snsList simplyblockv1alpha1.StorageNodeSetList
	if err := r.List(ctx, &snsList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(snsList.Items))
	for _, sns := range snsList.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: sns.Namespace, Name: sns.Name},
		})
	}
	return reqs
}

func (r *StorageNodeSetReconciler) tlsSecretToStorageNodeSetRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var snsList simplyblockv1alpha1.StorageNodeSetList
	if err := r.List(ctx, &snsList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(snsList.Items))
	for _, sns := range snsList.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: sns.Namespace, Name: sns.Name},
		})
	}
	return reqs
}

func (r *StorageNodeSetReconciler) spdkProxyPodToStorageNodeSetRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var snsList simplyblockv1alpha1.StorageNodeSetList
	if err := r.List(ctx, &snsList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(snsList.Items))
	for _, sns := range snsList.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: sns.Namespace, Name: sns.Name},
		})
	}
	return reqs
}

func (r *StorageNodeSetReconciler) handleDeletion(
	ctx context.Context,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) (bool, error) {
	if snsCR.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if !controllerutil.ContainsFinalizer(snsCR, utils.FinalizerStorageNode) {
		return true, nil
	}
	controllerutil.RemoveFinalizer(snsCR, utils.FinalizerStorageNode)
	return true, r.Update(ctx, snsCR)
}

func (r *StorageNodeSetReconciler) ensureFinalizer(
	ctx context.Context,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) (bool, error) {
	if controllerutil.ContainsFinalizer(snsCR, utils.FinalizerStorageNode) {
		return false, nil
	}
	controllerutil.AddFinalizer(snsCR, utils.FinalizerStorageNode)
	return true, r.Update(ctx, snsCR)
}

func (r *StorageNodeSetReconciler) labelWorkerNodes(ctx context.Context, sns *simplyblockv1alpha1.StorageNodeSet) error {
	for _, nodeName := range sns.Spec.WorkerNodes {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			return err
		}
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		key := "io.simplyblock.node-type"
		value := "simplyblock-storage-plane-" + sns.Spec.ClusterName
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

func (r *StorageNodeSetReconciler) labelWorkerNode(ctx context.Context, sns *simplyblockv1alpha1.StorageNodeSet) error {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: sns.Spec.WorkerNode}, &node); err != nil {
		return err
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	key := "io.simplyblock.node-type"
	value := "simplyblock-storage-plane-" + sns.Spec.ClusterName
	node.Labels[key] = value
	return r.Update(ctx, &node)
}

func (r *StorageNodeSetReconciler) reconcileDaemonSet(
	ctx context.Context,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	snAdapter := snSetToSN(snsCR)

	if snsCR.Spec.ClusterImage == "" {
		cp := &simplyblockv1alpha1.ControlPlane{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: SingletonControlPlaneName}, cp); err != nil {
			return fmt.Errorf("clusterImage not set and ControlPlane %q not found: %w", SingletonControlPlaneName, err)
		}
		if cp.Spec.Image == "" {
			return fmt.Errorf("clusterImage not set and ControlPlane %q has no spec.image", SingletonControlPlaneName)
		}
		snAdapter = snAdapter.DeepCopy()
		snAdapter.Spec.ClusterImage = cp.Spec.Image
	}

	tlsSecretRV, err := r.getTLSSecretResourceVersion(ctx, snsCR.Namespace)
	if err != nil {
		return err
	}

	ds := utils.BuildStorageNodeDaemonSet(snAdapter, r.TLSEnabled, r.TLSMutualEnabled, r.TLSProvider, tlsSecretRV)

	if err := controllerutil.SetControllerReference(snsCR, ds, r.Scheme); err != nil {
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

func (r *StorageNodeSetReconciler) getTLSSecretResourceVersion(ctx context.Context, namespace string) (string, error) {
	if !r.TLSEnabled {
		return "", nil
	}
	var sec corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: utils.SecretNameStorageNodeAPITLS}, &sec)
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
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	svc := utils.BuildStorageNodeService(snSetToSN(snsCR), r.TLSEnabled, r.TLSProvider)
	if err := controllerutil.SetControllerReference(snsCR, svc, r.Scheme); err != nil {
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
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	if !r.TLSEnabled || !utils.IsCertManagerTLSProvider(r.TLSProvider) {
		return nil
	}
	certificates := []struct {
		serviceName string
		secretName  string
	}{
		{serviceName: "simplyblock-storage-node-api", secretName: utils.SecretNameStorageNodeAPITLS},
		{serviceName: "simplyblock-spdk-proxy", secretName: utils.SecretNameSpdkProxyTLS},
	}
	for _, cert := range certificates {
		if err := r.reconcileServingCertificate(ctx, snsCR, cert.serviceName, cert.secretName); err != nil {
			return err
		}
	}
	return nil
}

func (r *StorageNodeSetReconciler) reconcileServingCertificate(
	ctx context.Context,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
	serviceName, secretName string,
) error {
	cert := utils.BuildServiceServingCertificate(snsCR.Namespace, serviceName, secretName)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		desired := utils.BuildServiceServingCertificate(snsCR.Namespace, serviceName, secretName)
		cert.Object["spec"] = desired.Object["spec"]
		return controllerutil.SetControllerReference(snsCR, cert, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to apply serving Certificate for %s: %w", serviceName, err)
	}
	return nil
}

func (r *StorageNodeSetReconciler) reconcileEndpointSlice(
	ctx context.Context,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	log := logf.FromContext(ctx)

	nodeIPs := make(map[string]string)
	for _, nodeName := range snsCR.Spec.WorkerNodes {
		ip, err := getNodeInternalIP(ctx, r.Client, nodeName)
		if err != nil {
			log.Error(err, "failed to get internal IP for EndpointSlice, skipping node", "node", nodeName)
			continue
		}
		nodeIPs[nodeName] = ip
	}

	eps := utils.BuildStorageNodeEndpointSlice(snSetToSN(snsCR), nodeIPs)
	if err := controllerutil.SetControllerReference(snsCR, eps, r.Scheme); err != nil {
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
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	svc := utils.BuildSpdkProxyService(snSetToSN(snsCR), r.TLSEnabled, r.TLSProvider)
	if err := controllerutil.SetControllerReference(snsCR, svc, r.Scheme); err != nil {
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
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	log := logf.FromContext(ctx)

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(snsCR.Namespace),
		client.MatchingLabels{"role": "simplyblock-storage-node"},
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
		eps, err := utils.BuildSpdkProxyEndpointSlice(snSetToSN(snsCR), rpcPort, endpoints)
		if err != nil {
			return err
		}
		if err := controllerutil.SetControllerReference(snsCR, eps, r.Scheme); err != nil {
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

	var existingSlices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &existingSlices,
		client.InNamespace(snsCR.Namespace),
		client.MatchingLabels{"kubernetes.io/service-name": "simplyblock-spdk-proxy"},
	); err != nil {
		return fmt.Errorf("failed to list existing spdk-proxy EndpointSlices: %w", err)
	}
	for i := range existingSlices.Items {
		slice := &existingSlices.Items[i]
		if !metav1.IsControlledBy(slice, snsCR) {
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

func (r *StorageNodeSetReconciler) pollNodeSetOnline(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, ip, nodeName string,
	expectedPerHost int,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)

	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	log.Info("SNODE LIST raw API response", "endpoint", endpoint, "status", status, "body", string(body))

	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Failed to get storage node statuses", "node", nodeName, "status", status, "response", string(body))
		return ctrl.Result{RequeueAfter: waitForNodeOnlineWaitInterval}, nil
	}

	if string(body) == "[]" {
		log.Info("Storage node list is empty", "node", nodeName)
		return r.nodeSetOnlineRequeueOrTimeout(ctx, nodeName, ip, snsCR)
	}

	var apiResp []SNODEAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to unmarshal storage node response for %s: %v", nodeName, err)
	}

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
		return r.nodeSetOnlineRequeueOrTimeout(ctx, nodeName, ip, snsCR)
	}

	if err := onAllSocketNodeSetNodesOnline(ctx, apiClient, clusterSecret, clusterUUID, snsCR, nodeName, onlineForHost, r); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Storage node set created successfully", "node", nodeName)
	return ctrl.Result{}, nil
}

func (r *StorageNodeSetReconciler) nodeSetOnlineRequeueOrTimeout(
	ctx context.Context,
	nodeName, ip string,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	timeout := time.Duration(waitForNodeOnlineRetries) * waitForNodeOnlineWaitInterval

	for i := range snsCR.Status.Nodes {
		n := &snsCR.Status.Nodes[i]
		if n.Hostname == nodeName && n.UUID == "" && n.PostedAt != nil {
			if time.Since(n.PostedAt.Time) <= timeout {
				return ctrl.Result{RequeueAfter: waitForNodeOnlineWaitInterval}, nil
			}
		}
	}

	log.Error(nil, "Timeout waiting for node to become online", "node", nodeName)
	updated := false
	for i := range snsCR.Status.Nodes {
		if snsCR.Status.Nodes[i].Hostname == nodeName {
			snsCR.Status.Nodes[i].Status = "timeout"
			snsCR.Status.Nodes[i].MgmtIp = ip
			updated = true
		}
	}
	if !updated {
		snsCR.Status.Nodes = append(snsCR.Status.Nodes, simplyblockv1alpha1.NodeStatus{
			Hostname: nodeName,
			MgmtIp:   ip,
			Status:   "timeout",
		})
	}
	if err := r.Status().Update(ctx, snsCR); err != nil {
		log.Error(err, "Failed to update node status after timeout", "node", nodeName)
	}
	return ctrl.Result{}, nil
}

func onAllSocketNodeSetNodesOnline(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID string,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
	nodeName string,
	onlineForHost []SNODEAPIResponse,
	r *StorageNodeSetReconciler,
) error {
	log := logf.FromContext(ctx)

	patch := client.MergeFrom(snsCR.DeepCopy())
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

		matched := false
		for i := range snsCR.Status.Nodes {
			n := &snsCR.Status.Nodes[i]
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
			snsCR.Status.Nodes = append(snsCR.Status.Nodes, updated)
			changed = true
		}
	}

	if changed {
		if err := r.Status().Patch(ctx, snsCR, patch); err != nil {
			log.Error(err, "Failed to patch node set status to online", "node", nodeName)
		}
	}

	log.Info("All socket nodes online", "node", nodeName, "count", len(onlineForHost))

	return maybeActivateNodeSetCluster(ctx, apiClient, clusterSecret, clusterUUID, snsCR, r)
}

func (r *StorageNodeSetReconciler) syncTrackedNodesStatus(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID string,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	log := logf.FromContext(ctx)

	hasTracked := false
	for _, n := range snsCR.Status.Nodes {
		if n.UUID != "" {
			hasTracked = true
			break
		}
	}
	if !hasTracked {
		return nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
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

	patch := client.MergeFrom(snsCR.DeepCopy())
	changed := false

	for i := range snsCR.Status.Nodes {
		n := &snsCR.Status.Nodes[i]
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
		if err := r.Status().Patch(ctx, snsCR, patch); err != nil {
			log.Error(err, "Failed to patch storage node set status during sync")
			return err
		}
		log.Info("Storage node set status synced")
	}
	return nil
}

func maybeActivateNodeSetCluster(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID string,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
	r *StorageNodeSetReconciler,
) error {
	log := logf.FromContext(ctx)

	clusterCR, err := utils.ResolveClusterCR(ctx, r.Client, snsCR.Namespace, snsCR.Spec.ClusterName)
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

	onlineHealthy := utils.CountOnlineHealthyNodes(snsCR.Status.Nodes)
	log.Info("Evaluating cluster activation conditions",
		"erasureCodingScheme", clusterCR.Status.ErasureCodingScheme,
		"onlineHealthy", onlineHealthy,
	)

	requiredEc, err := utils.RequiredNodesFromErasureCodingScheme(clusterCR.Status.ErasureCodingScheme)
	if err != nil {
		log.Error(err, "Invalid erasure coding scheme")
		return err
	}

	if utils.ShouldActivateCluster(requiredEc, onlineHealthy, snSetToSN(snsCR)) {
		waitForNodeOnlineSleepFn(waitForNodeOnlineActivationDelay)
		log.Info("Activation conditions met — activating cluster")
		if err := utils.ActivateClusterAndWait(ctx, apiClient, clusterSecret, clusterUUID); err != nil {
			log.Error(err, "Cluster activation did not complete")
			return err
		}
		log.Info("Cluster successfully activated")
	}

	return nil
}

func ensureNodeSetStatus(
	snsCR *simplyblockv1alpha1.StorageNodeSet,
	nodeName, ip string,
) *simplyblockv1alpha1.NodeStatus {
	for i := range snsCR.Status.Nodes {
		if snsCR.Status.Nodes[i].Hostname == nodeName {
			return &snsCR.Status.Nodes[i]
		}
	}
	now := metav1.Now()
	snsCR.Status.Nodes = append(snsCR.Status.Nodes, simplyblockv1alpha1.NodeStatus{
		Hostname: nodeName,
		MgmtIp:   ip,
		Status:   "in_creation",
		PostedAt: &now,
	})
	return &snsCR.Status.Nodes[len(snsCR.Status.Nodes)-1]
}

func (r *StorageNodeSetReconciler) reconcileAction(
	ctx context.Context,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
	clusterUUID string,
	clusterSecret string,
) (ctrl.Result, error) {
	apiClient := webapi.NewClient()
	if err := r.handleNodeSetAction(ctx, apiClient, snsCR, clusterUUID, clusterSecret); err != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *StorageNodeSetReconciler) handleNodeSetAction(
	ctx context.Context,
	apiClient *webapi.Client,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
	clusterUUID, clusterSecret string,
) error {
	log := logf.FromContext(ctx)

	if snsCR.Status.ActionStatus != nil &&
		snsCR.Status.ActionStatus.Action == snsCR.Spec.Action &&
		snsCR.Status.ActionStatus.NodeUUID == snsCR.Spec.NodeUUID &&
		snsCR.Status.ActionStatus.State == utils.ActionStateSuccess {
		log.Info("Action already completed successfully, skipping",
			"action", snsCR.Spec.Action,
			"nodeUUID", snsCR.Spec.NodeUUID,
		)
		return nil
	}

	snsCR.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
		Action:    snsCR.Spec.Action,
		NodeUUID:  snsCR.Spec.NodeUUID,
		State:     utils.ActionStateRunning,
		UpdatedAt: metav1.Now(),
	}
	if err := r.Status().Update(ctx, snsCR); err != nil {
		log.Error(err, "Failed to set action status to running")
		return err
	}

	if err := r.performNodeSetAction(ctx, apiClient, clusterUUID, clusterSecret, snsCR); err != nil {
		log.Error(err, "Action failed", "action", snsCR.Spec.Action, "nodeUUID", snsCR.Spec.NodeUUID)
		snsCR.Status.ActionStatus.State = utils.ActionStateFailed
		snsCR.Status.ActionStatus.Message = err.Error()
		snsCR.Status.ActionStatus.UpdatedAt = metav1.Now()
		_ = r.Status().Update(ctx, snsCR)
		return err
	}

	snsCR.Status.ActionStatus.State = utils.ActionStateSuccess
	snsCR.Status.ActionStatus.Message = "Action executed successfully"
	snsCR.Status.ActionStatus.UpdatedAt = metav1.Now()
	if err := r.Status().Update(ctx, snsCR); err != nil {
		log.Error(err, "Failed to update action status")
		return err
	}

	log.Info("Action completed successfully", "action", snsCR.Spec.Action, "nodeUUID", snsCR.Spec.NodeUUID)
	return nil
}

func (r *StorageNodeSetReconciler) performNodeSetAction(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	clusterSecret string,
	snsCR *simplyblockv1alpha1.StorageNodeSet,
) error {
	log := logf.FromContext(ctx)

	var (
		endpoint string
		method   = http.MethodPost
		body     any
	)

	switch snsCR.Spec.Action {
	case "restart":
		payload := map[string]any{
			"force":           nodeActionForce(snSetToSN(snsCR), true),
			"reattach_volume": utils.BoolPtrOrFalse(snsCR.Spec.ReattachVolume),
		}

		if snsCR.Spec.WorkerNode != "" {
			if err := r.labelWorkerNode(ctx, snsCR); err != nil {
				return fmt.Errorf("failed to label worker node %s: %w", snsCR.Spec.WorkerNode, err)
			}
			if err := waitForNodeInfoReachable(ctx, snsCR.Spec.WorkerNode, snsCR.Namespace, r.TLSEnabled, r.TLSMutualEnabled); err != nil {
				log.Error(err, "node never became reachable")
				return err
			}
			body = map[string]any{
				"force":           nodeActionForce(snSetToSN(snsCR), true),
				"reattach_volume": utils.BoolPtrOrFalse(snsCR.Spec.ReattachVolume),
				"node_address":    utils.StorageNodeAPIAddress(snsCR.Spec.WorkerNode, snsCR.Namespace),
			}
		} else {
			body = payload
		}

		endpoint = fmt.Sprintf(
			"/api/v2/clusters/%s/storage-nodes/%s/restart",
			clusterUUID,
			snsCR.Spec.NodeUUID,
		)

	case "remove":
		method = http.MethodDelete
		body = nil
		endpoint = fmt.Sprintf(
			"/api/v2/clusters/%s/storage-nodes/%s?force_remove=%t",
			clusterUUID,
			snsCR.Spec.NodeUUID,
			nodeActionForce(snSetToSN(snsCR), true),
		)

	default:
		body = nil
		endpoint = fmt.Sprintf(
			"/api/v2/clusters/%s/storage-nodes/%s/%s",
			clusterUUID,
			snsCR.Spec.NodeUUID,
			snsCR.Spec.Action,
		)
	}

	respBody, status, err := apiClient.Do(ctx, clusterSecret, method, endpoint, body)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Node action API call failed", "action", snsCR.Spec.Action, "nodeUUID", snsCR.Spec.NodeUUID, "status", status, "response", string(respBody))
		return fmt.Errorf("action API failed: status=%d err=%v", status, err)
	}

	log.Info("Node action triggered",
		"nodeUUID", snsCR.Spec.NodeUUID,
		"action", snsCR.Spec.Action,
		"response", string(respBody),
	)

	performNodeActionSleepFn(performNodeActionPostTriggerDelay)

	if err := r.waitForNodeSetActionCompletion(ctx, apiClient, clusterUUID, clusterSecret, snsCR.Spec.NodeUUID, snsCR.Spec.Action); err != nil {
		return fmt.Errorf("node did not reach expected state after action %s: %w", snsCR.Spec.Action, err)
	}

	log.Info("Node reached expected state",
		"nodeUUID", snsCR.Spec.NodeUUID,
		"action", snsCR.Spec.Action,
	)
	return nil
}

func (r *StorageNodeSetReconciler) waitForNodeSetActionCompletion(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	clusterSecret string,
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

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterUUID, nodeUUID)

	for i := 0; i < waitForActionCompletionRetries; i++ {
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)

		if action == "remove" && status == http.StatusNotFound {
			log.Info("Node successfully removed (404 returned)", "nodeUUID", nodeUUID)
			return nil
		}

		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d", status)
			}
			log.Error(err, "Failed to get node status",
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
			log.Info("Node reached expected status", "nodeUUID", nodeUUID, "status", resp.Status)
			return nil
		}

		waitForActionCompletionSleepFn(waitForActionCompletionWaitInterval)
	}

	return fmt.Errorf("node %s did not reach expected status %q after action %q", nodeUUID, targetStatus, action)
}
