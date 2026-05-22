package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	statusOnline  = "online"
	mgmtIP        = "10.0.0.1"
	tlsVolumeName = "tls"
	caVolumeName  = "certificate-authority"
)

func TestEnsureNodeStatus(t *testing.T) {
	cr := &simplyblockv1alpha1.StorageNode{}

	s := ensureNodeStatus(cr, "node-a", mgmtIP)
	if s == nil {
		t.Fatalf("ensureNodeStatus returned nil")
	}
	if s.Hostname != "node-a" || s.MgmtIp != mgmtIP || s.Status != "in_creation" {
		t.Fatalf("unexpected initial node status: %#v", *s)
	}
	if len(cr.Status.Nodes) != 1 {
		t.Fatalf("expected one node, got %d", len(cr.Status.Nodes))
	}

	s2 := ensureNodeStatus(cr, "node-a", "10.0.0.99")
	if s2 == nil {
		t.Fatalf("ensureNodeStatus second call returned nil")
	}
	if len(cr.Status.Nodes) != 1 {
		t.Fatalf("should not append duplicate node entry")
	}
	// existing value should be retained
	if s2.MgmtIp != mgmtIP {
		t.Fatalf("expected existing node status to be reused, got %#v", *s2)
	}
}

func TestWaitForActionCompletionUnknownAction(t *testing.T) {
	r := &StorageNodeReconciler{}
	c := webapi.NewClient("http://127.0.0.1:1")
	err := r.waitForActionCompletion(context.Background(), c, "cluster", "secret", "node", "invalid-action")
	if err == nil {
		t.Fatalf("expected error for unknown action")
	}
}

func TestWaitForActionCompletionValidTransitions(t *testing.T) {
	tests := []struct {
		name       string
		action     string
		statusCode int
		respStatus string
	}{
		{
			name:       "suspend reaches suspended",
			action:     "suspend",
			statusCode: http.StatusOK,
			respStatus: "suspended",
		},
		{
			name:       "resume reaches online",
			action:     "resume",
			statusCode: http.StatusOK,
			respStatus: statusOnline,
		},
		{
			name:       "shutdown reaches offline",
			action:     "shutdown",
			statusCode: http.StatusOK,
			respStatus: "offline",
		},
		{
			name:       "restart reaches online",
			action:     "restart",
			statusCode: http.StatusOK,
			respStatus: statusOnline,
		},
		{
			name:       "remove reaches deleted via 404",
			action:     "remove",
			statusCode: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.statusCode == http.StatusNotFound {
					w.WriteHeader(http.StatusNotFound)
					return
				}

				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(utils.NodeStatusResponse{
					Status: tc.respStatus,
				})
			}))
			defer srv.Close()

			r := &StorageNodeReconciler{}
			c := webapi.NewClient(srv.URL)
			err := r.waitForActionCompletion(context.Background(), c, "cluster", "secret", "node", tc.action)
			if err != nil {
				t.Fatalf("waitForActionCompletion returned error: %v", err)
			}
		})
	}
}

func TestHandleNodeActionTransitions(t *testing.T) {
	t.Run("does not re-enter terminal success for same action and node", func(t *testing.T) {
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-a",
				Namespace: "default",
			},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				Action:   "restart",
				NodeUUID: "node-1",
			},
			Status: simplyblockv1alpha1.StorageNodeStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action:   "restart",
					NodeUUID: "node-1",
					State:    utils.ActionStateSuccess,
				},
			},
		}

		r := newStorageNodeStateTestReconciler(t, sn)
		err := r.handleNodeAction(context.Background(), webapi.NewClient("http://127.0.0.1:1"), sn, "cluster", "secret")
		if err != nil {
			t.Fatalf("handleNodeAction returned error: %v", err)
		}
		if sn.Status.ActionStatus.State != utils.ActionStateSuccess {
			t.Fatalf("expected success to remain stable, got %q", sn.Status.ActionStatus.State)
		}
	})

	t.Run("transitions running to failed when action call fails", func(t *testing.T) {
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-b",
				Namespace: "default",
			},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				Action:   "restart",
				NodeUUID: "node-2",
			},
		}

		r := newStorageNodeStateTestReconciler(t, sn)
		err := r.handleNodeAction(context.Background(), webapi.NewClient("http://127.0.0.1:1"), sn, "cluster", "secret")
		if err == nil {
			t.Fatalf("expected action failure")
		}

		current := &simplyblockv1alpha1.StorageNode{}
		if getErr := r.Get(context.Background(), client.ObjectKeyFromObject(sn), current); getErr != nil {
			t.Fatalf("failed to fetch storagenode: %v", getErr)
		}
		if current.Status.ActionStatus == nil {
			t.Fatalf("expected action status")
		}
		if current.Status.ActionStatus.State != utils.ActionStateFailed {
			t.Fatalf("expected failed state, got %q", current.Status.ActionStatus.State)
		}
		if strings.TrimSpace(current.Status.ActionStatus.Message) == "" {
			t.Fatalf("expected failure message to be set")
		}
	})
}

func TestHandleNodeActionRejectsIllegalSuccessIdentity(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-illegal-success",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			Action:   "restart",
			NodeUUID: "node-expected",
		},
		Status: simplyblockv1alpha1.StorageNodeStatus{
			// Illegal success for another identity should not be accepted.
			ActionStatus: &simplyblockv1alpha1.ActionStatus{
				Action:   "restart",
				NodeUUID: "node-other",
				State:    utils.ActionStateSuccess,
			},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn)
	err := r.handleNodeAction(context.Background(), webapi.NewClient("http://127.0.0.1:1"), sn, "cluster", "secret")
	if err == nil {
		t.Fatalf("expected failure after rejecting illegal success identity")
	}

	current := &simplyblockv1alpha1.StorageNode{}
	if getErr := r.Get(context.Background(), client.ObjectKeyFromObject(sn), current); getErr != nil {
		t.Fatalf("failed to fetch storagenode: %v", getErr)
	}
	if current.Status.ActionStatus == nil {
		t.Fatalf("expected action status")
	}
	if current.Status.ActionStatus.State != utils.ActionStateFailed {
		t.Fatalf("expected stale/illegal success to be rejected and transitioned to failed, got %q", current.Status.ActionStatus.State)
	}
	if strings.TrimSpace(current.Status.ActionStatus.Message) == "" {
		t.Fatalf("expected failure message to be set")
	}
}

func TestStorageNodeFinalizerLifecycleHelpers(t *testing.T) {
	now := metav1.NewTime(time.Now())

	t.Run("ensureFinalizer adds finalizer when missing", func(t *testing.T) {
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-finalizer-add",
				Namespace: "default",
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		updated, err := r.ensureFinalizer(context.Background(), sn)
		if err != nil {
			t.Fatalf("ensureFinalizer returned error: %v", err)
		}
		if !updated {
			t.Fatalf("expected ensureFinalizer to report update")
		}
		if !contains(sn.Finalizers, utils.FinalizerStorageNode) {
			t.Fatalf("expected storagenode finalizer to be set")
		}
	})

	t.Run("handleDeletion removes finalizer when deletion timestamp is set", func(t *testing.T) {
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "sn-finalizer-del",
				Namespace:         "default",
				Finalizers:        []string{utils.FinalizerStorageNode},
				DeletionTimestamp: &now,
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		updated, err := r.handleDeletion(context.Background(), sn)
		if err != nil {
			t.Fatalf("handleDeletion returned error: %v", err)
		}
		if !updated {
			t.Fatalf("expected handleDeletion to report update")
		}
		if contains(sn.Finalizers, utils.FinalizerStorageNode) {
			t.Fatalf("expected storagenode finalizer to be removed")
		}
	})
}

func TestStorageNodeLabelingHelpers(t *testing.T) {
	t.Run("labelWorkerNodes labels all configured workers", func(t *testing.T) {
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-label-all",
				Namespace: "default",
			},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				ClusterName: "cluster-a",
				WorkerNodes: []string{"node-a", "node-b"},
			},
		}
		nodeA := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
		nodeB := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}
		r := newStorageNodeStateTestReconciler(t, sn, nodeA, nodeB)

		if err := r.labelWorkerNodes(context.Background(), sn); err != nil {
			t.Fatalf("labelWorkerNodes returned error: %v", err)
		}

		for _, nodeName := range []string{"node-a", "node-b"} {
			var n corev1.Node
			if err := r.Get(context.Background(), client.ObjectKey{Name: nodeName}, &n); err != nil {
				t.Fatalf("failed to fetch node %s: %v", nodeName, err)
			}
			got := n.Labels["io.simplyblock.node-type"]
			want := "simplyblock-storage-plane-cluster-a"
			if got != want {
				t.Fatalf("node %s label mismatch: got %q want %q", nodeName, got, want)
			}
		}
	})

	t.Run("labelWorkerNode labels single worker node", func(t *testing.T) {
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-label-one",
				Namespace: "default",
			},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				ClusterName: "cluster-b",
				WorkerNode:  "node-one",
			},
		}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-one"}}
		r := newStorageNodeStateTestReconciler(t, sn, node)

		if err := r.labelWorkerNode(context.Background(), sn); err != nil {
			t.Fatalf("labelWorkerNode returned error: %v", err)
		}

		var out corev1.Node
		if err := r.Get(context.Background(), client.ObjectKey{Name: "node-one"}, &out); err != nil {
			t.Fatalf("failed to fetch node: %v", err)
		}
		if out.Labels["io.simplyblock.node-type"] != "simplyblock-storage-plane-cluster-b" {
			t.Fatalf("expected worker node label to be set")
		}
	})
}

func TestStorageNodeDaemonSetReconcileCreatesWhenMissing(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-ds-create",
			Namespace: "default",
			UID:       "uid-create",
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
	}
	r := newStorageNodeStateTestReconciler(t, sn)

	if err := r.reconcileDaemonSet(context.Background(), sn); err != nil {
		t.Fatalf("reconcileDaemonSet returned error: %v", err)
	}

	var ds appsv1.DaemonSet
	if err := r.Get(context.Background(), client.ObjectKey{Name: "simplyblock-storage-node-ds-cluster-a", Namespace: "default"}, &ds); err != nil {
		t.Fatalf("daemonset should be created: %v", err)
	}
	if len(ds.OwnerReferences) == 0 || ds.OwnerReferences[0].Name != sn.Name {
		t.Fatalf("expected daemonset to be owned by storagenode")
	}
}

func TestStorageNodeDaemonSetReconcileUpdatesExisting(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-ds-update",
			Namespace: "default",
			UID:       "uid-update",
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
	}
	existing := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-storage-node-ds-cluster-a",
			Namespace: "default",
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn, existing)

	if err := r.reconcileDaemonSet(context.Background(), sn); err != nil {
		t.Fatalf("reconcileDaemonSet returned error: %v", err)
	}

	var ds appsv1.DaemonSet
	if err := r.Get(context.Background(), client.ObjectKey{Name: "simplyblock-storage-node-ds-cluster-a", Namespace: "default"}, &ds); err != nil {
		t.Fatalf("failed to fetch daemonset: %v", err)
	}
	if len(ds.OwnerReferences) == 0 || ds.OwnerReferences[0].Name != sn.Name {
		t.Fatalf("expected updated daemonset to carry owner reference")
	}
}

func TestStorageNodeDaemonSetReconcileTLSDisabled(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-ds-tls-off", Namespace: "default", UID: "uid-tls-off"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
	}
	r := newStorageNodeStateTestReconciler(t, sn)
	r.TLSEnabled = false

	if err := r.reconcileDaemonSet(context.Background(), sn); err != nil {
		t.Fatalf("reconcileDaemonSet returned error: %v", err)
	}

	var ds appsv1.DaemonSet
	if err := r.Get(context.Background(), client.ObjectKey{Name: "simplyblock-storage-node-ds-cluster-a", Namespace: "default"}, &ds); err != nil {
		t.Fatalf("failed to fetch daemonset: %v", err)
	}
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.Name == tlsVolumeName || v.Name == caVolumeName {
			t.Fatalf("unexpected TLS volume present: %s", v.Name)
		}
	}
	for _, c := range ds.Spec.Template.Spec.InitContainers {
		for _, m := range c.VolumeMounts {
			if m.Name == tlsVolumeName || m.Name == caVolumeName {
				t.Fatalf("unexpected TLS mount on init container: %s", m.Name)
			}
		}
	}
	for _, c := range ds.Spec.Template.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == tlsVolumeName || m.Name == caVolumeName {
				t.Fatalf("unexpected TLS mount on main container: %s", m.Name)
			}
		}
		if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil {
			t.Fatalf("expected HTTPGet readiness probe")
		}
		if c.ReadinessProbe.HTTPGet.Scheme != "" && c.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTP {
			t.Fatalf("expected default/HTTP probe scheme when TLS disabled, got %q", c.ReadinessProbe.HTTPGet.Scheme)
		}
		if _, ok := envValue(c.Env, "SB_TLS_CONNECT"); ok {
			t.Fatalf("unexpected SB_TLS_CONNECT env when TLS disabled")
		}
	}
}

func checkTLSMounts(t *testing.T, label string, mounts []corev1.VolumeMount) {
	t.Helper()
	var gotTLS bool
	for _, m := range mounts {
		switch m.Name {
		case tlsVolumeName:
			gotTLS = true
			if m.MountPath != "/etc/simplyblock/tls" || m.SubPath != "" || !m.ReadOnly {
				t.Fatalf("%s: tls mount shape wrong: %#v", label, m)
			}
		case caVolumeName:
			t.Fatalf("%s: unexpected separate certificate-authority mount: %#v", label, m)
		}
	}
	if !gotTLS {
		t.Fatalf("%s: expected tls mount", label)
	}
}

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, item := range env {
		if item.Name == name {
			return item.Value, true
		}
	}
	return "", false
}

func TestStorageNodeDaemonSetReconcileTLSEnabled(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-ds-tls-on", Namespace: "default", UID: "uid-tls-on"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
	}
	r := newStorageNodeStateTestReconciler(t, sn)
	r.TLSEnabled = true
	r.TLSProvider = utils.TLSProviderOpenShift
	r.TLSMutualEnabled = true

	if err := r.reconcileDaemonSet(context.Background(), sn); err != nil {
		t.Fatalf("reconcileDaemonSet returned error: %v", err)
	}

	var ds appsv1.DaemonSet
	if err := r.Get(context.Background(), client.ObjectKey{Name: "simplyblock-storage-node-ds-cluster-a", Namespace: "default"}, &ds); err != nil {
		t.Fatalf("failed to fetch daemonset: %v", err)
	}

	var tlsVol *corev1.Volume
	for i := range ds.Spec.Template.Spec.Volumes {
		v := &ds.Spec.Template.Spec.Volumes[i]
		switch v.Name {
		case tlsVolumeName:
			tlsVol = v
		case caVolumeName:
			t.Fatalf("unexpected separate certificate-authority volume: %#v", v)
		}
	}
	if tlsVol == nil || tlsVol.Projected == nil {
		t.Fatalf("expected projected tls volume, got %#v", tlsVol)
	}
	var gotSecret, gotCA bool
	for _, src := range tlsVol.Projected.Sources {
		switch {
		case src.Secret != nil && src.Secret.Name == "simplyblock-storage-node-api-tls":
			gotSecret = true
		case src.ConfigMap != nil && src.ConfigMap.Name == "simplyblock-certificate-authority":
			gotCA = true
			if len(src.ConfigMap.Items) != 1 || src.ConfigMap.Items[0].Key != "service-ca.crt" || src.ConfigMap.Items[0].Path != "ca.crt" {
				t.Fatalf("ca configmap projection wrong: %#v", src.ConfigMap.Items)
			}
		}
	}
	if !gotSecret || !gotCA {
		t.Fatalf("expected projected sources for secret and ca configmap, got secret=%v ca=%v", gotSecret, gotCA)
	}

	if len(ds.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected single init container")
	}
	checkTLSMounts(t, "init container", ds.Spec.Template.Spec.InitContainers[0].VolumeMounts)
	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected single main container")
	}
	checkTLSMounts(t, "main container", ds.Spec.Template.Spec.Containers[0].VolumeMounts)

	probe := ds.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.TCPSocket == nil {
		t.Fatalf("expected TCPSocket readiness probe under mutual TLS, got %#v", probe)
	}
	if probe.HTTPGet != nil {
		t.Fatalf("did not expect HTTPGet readiness probe under mutual TLS, got %#v", probe.HTTPGet)
	}
	if got, ok := envValue(ds.Spec.Template.Spec.Containers[0].Env, "SB_TLS_CONNECT"); !ok || got != "authenticated" {
		t.Fatalf("expected SB_TLS_CONNECT=authenticated on main container, got value=%q present=%v", got, ok)
	}
}

func TestStorageNodeDaemonSetReconcileTLSCertManagerProvider(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-ds-tls-cert-manager", Namespace: "default", UID: "uid-tls-cert-manager"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
	}
	r := newStorageNodeStateTestReconciler(t, sn)
	r.TLSEnabled = true
	r.TLSProvider = utils.TLSProviderCertManager
	r.TLSMutualEnabled = false

	if err := r.reconcileDaemonSet(context.Background(), sn); err != nil {
		t.Fatalf("reconcileDaemonSet returned error: %v", err)
	}

	var ds appsv1.DaemonSet
	if err := r.Get(context.Background(), client.ObjectKey{Name: "simplyblock-storage-node-ds-cluster-a", Namespace: "default"}, &ds); err != nil {
		t.Fatalf("failed to fetch daemonset: %v", err)
	}

	var tlsVol *corev1.Volume
	for i := range ds.Spec.Template.Spec.Volumes {
		v := &ds.Spec.Template.Spec.Volumes[i]
		switch v.Name {
		case tlsVolumeName:
			tlsVol = v
		case caVolumeName:
			t.Fatalf("unexpected separate certificate-authority volume: %#v", v)
		}
	}
	if tlsVol == nil {
		t.Fatalf("expected tls volume, got none")
	}
	if tlsVol.Projected != nil {
		t.Fatalf("expected plain Secret volume for cert-manager provider, got projected: %#v", tlsVol.Projected)
	}
	if tlsVol.Secret == nil || tlsVol.Secret.SecretName != "simplyblock-storage-node-api-tls" {
		t.Fatalf("expected Secret volume referencing simplyblock-storage-node-api-tls, got %#v", tlsVol.Secret)
	}

	if len(ds.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected single init container")
	}
	checkTLSMounts(t, "init container", ds.Spec.Template.Spec.InitContainers[0].VolumeMounts)
	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected single main container")
	}
	checkTLSMounts(t, "main container", ds.Spec.Template.Spec.Containers[0].VolumeMounts)

	probe := ds.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.HTTPGet == nil {
		t.Fatalf("expected HTTPGet readiness probe under server-only TLS, got %#v", probe)
	}
	if probe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
		t.Fatalf("expected readiness probe scheme HTTPS, got %q", probe.HTTPGet.Scheme)
	}
}

func TestGetNodeInternalIP(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-ip"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "node-ip"},
				{Type: corev1.NodeInternalIP, Address: "10.1.2.3"},
			},
		},
	}
	r := newStorageNodeStateTestReconciler(t, node)

	got, err := getNodeInternalIP(context.Background(), r.Client, "node-ip")
	if err != nil {
		t.Fatalf("getNodeInternalIP returned error: %v", err)
	}
	if got != "10.1.2.3" {
		t.Fatalf("expected internal IP 10.1.2.3, got %q", got)
	}
}

func TestGetNodeInternalIPNoAddress(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-no-ip"},
	}
	r := newStorageNodeStateTestReconciler(t, node)

	_, err := getNodeInternalIP(context.Background(), r.Client, "node-no-ip")
	if err == nil {
		t.Fatalf("expected error when node has no internal IP")
	}
}

func TestStorageNodeReconcileActionFastPaths(t *testing.T) {
	t.Run("reconcileAction returns no requeue when action already successful", func(t *testing.T) {
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-ra-ok", Namespace: "default"},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				Action:   "restart",
				NodeUUID: "node-1",
			},
			Status: simplyblockv1alpha1.StorageNodeStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action:   "restart",
					NodeUUID: "node-1",
					State:    utils.ActionStateSuccess,
				},
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		res, err := r.reconcileAction(context.Background(), sn, "cluster", "secret")
		if err != nil {
			t.Fatalf("reconcileAction returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected no delayed requeue for successful action, got %+v", res)
		}
	})

	t.Run("reconcileAction requeues on action failure", func(t *testing.T) {
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-ra-fail", Namespace: "default"},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				Action:   "restart",
				NodeUUID: "node-2",
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		res, err := r.reconcileAction(context.Background(), sn, "cluster", "secret")
		if err != nil {
			t.Fatalf("reconcileAction returned unexpected error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue after failed action, got %+v", res)
		}
	})
}

func TestStorageNodeHandleDeletionNoopWithoutDeletionTimestamp(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-no-delete",
			Namespace: "default",
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn)

	updated, err := r.handleDeletion(context.Background(), sn)
	if err != nil {
		t.Fatalf("handleDeletion returned error: %v", err)
	}
	if updated {
		t.Fatalf("expected no update when deletion timestamp is zero")
	}
}

func TestStorageNodeHandleDeletionDoneWithoutFinalizer(t *testing.T) {
	now := metav1.NewTime(time.Now())
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sn-delete-done",
			Namespace:         "default",
			DeletionTimestamp: &now,
		},
	}
	r := newStorageNodeStateTestReconciler(t)

	updated, err := r.handleDeletion(context.Background(), sn)
	if err != nil {
		t.Fatalf("handleDeletion returned error: %v", err)
	}
	if !updated {
		t.Fatalf("expected deletion flow to be treated as handled without finalizer")
	}
}

func TestStorageNodeReconcileClusterUnavailableRequeues(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-reconcile-no-cluster",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-missing"},
	}
	r := newStorageNodeStateTestReconciler(t, sn)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue when cluster UUID is unavailable")
	}
}

func TestStorageNodeReconcileSecretMissingRequeues(t *testing.T) {
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: "cluster-uuid-no-secret"},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-reconcile-no-secret",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
	}
	r := newStorageNodeStateTestReconciler(t, sn, cluster)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue when cluster secret is unavailable")
	}
}

func TestStorageNodeReconcileNotFoundReturnsNil(t *testing.T) {
	r := newStorageNodeStateTestReconciler(t)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "missing", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for missing object, got %+v", res)
	}
}

func TestStorageNodeReconcileDeletionFlow(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-del"
	const clusterUUID = "cluster-uuid-del"
	now := metav1.NewTime(time.Now())

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sn-delete-flow",
			Namespace:         namespace,
			Finalizers:        []string{utils.FinalizerStorageNode},
			DeletionTimestamp: &now,
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{ClusterName: clusterName},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected deletion flow to complete without requeue, got %+v", res)
	}

	current := &simplyblockv1alpha1.StorageNode{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), current); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("failed to fetch storagenode: %v", err)
		}
		return
	}
	if contains(current.Finalizers, utils.FinalizerStorageNode) {
		t.Fatalf("expected finalizer to be removed during deletion flow")
	}
}

func TestStorageNodeReconcileAddsFinalizer(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-finalizer"
	const clusterUUID = "cluster-uuid-finalizer"

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-finalizer-flow",
			Namespace: namespace,
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{ClusterName: clusterName},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected finalizer add path to return without requeue, got %+v", res)
	}

	current := &simplyblockv1alpha1.StorageNode{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), current); err != nil {
		t.Fatalf("failed to fetch storagenode: %v", err)
	}
	if !contains(current.Finalizers, utils.FinalizerStorageNode) {
		t.Fatalf("expected finalizer to be added by reconcile")
	}
}

func TestStorageNodeReconcileActionPath(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-action"
	const clusterUUID = "cluster-uuid-action"

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-action-flow",
			Namespace:  namespace,
			Finalizers: []string{utils.FinalizerStorageNode},
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			ClusterName: clusterName,
			Action:      "restart",
			NodeUUID:    "node-action-1",
		},
		Status: simplyblockv1alpha1.StorageNodeStatus{
			ActionStatus: &simplyblockv1alpha1.ActionStatus{
				Action:   "restart",
				NodeUUID: "node-action-1",
				State:    utils.ActionStateSuccess,
			},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected action fast-path to avoid delayed requeue, got %+v", res)
	}
}

func TestStorageNodeReconcileLabelWorkerNodesFailure(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-label-fail"
	const clusterUUID = "cluster-uuid-label-fail"

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-label-fail",
			Namespace:  namespace,
			Finalizers: []string{utils.FinalizerStorageNode},
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			ClusterName: clusterName,
			WorkerNodes: []string{"missing-worker"},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err == nil {
		t.Fatalf("expected reconcile to fail when worker node lookup fails")
	}
}

func TestStorageNodeReconcileKnownWorkerSkipsProvisioning(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-known-worker"
	const clusterUUID = "cluster-uuid-known-worker"
	const workerName = "node-known"

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: workerName},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-known-worker",
			Namespace:  namespace,
			Finalizers: []string{utils.FinalizerStorageNode},
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			ClusterName: clusterName,
			WorkerNodes: []string{workerName},
		},
		Status: simplyblockv1alpha1.StorageNodeStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{
					Hostname: workerName,
					MgmtIp:   "10.0.0.10",
					Status:   statusOnline,
					UUID:     "node-uuid-known",
				},
			},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret, node)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no delayed requeue when worker already known, got %+v", res)
	}
}

func TestStorageNodeReconcileServiceAccountHasOwnerReference(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-ownerref-sa"
	const clusterUUID = "cluster-uuid-ownerref-sa"

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-ownerref-sa",
			Namespace:  namespace,
			Finalizers: []string{utils.FinalizerStorageNode},
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			ClusterName: clusterName,
			WorkerNodes: []string{},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	sa := &corev1.ServiceAccount{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Name:      "simplyblock-storage-node-sa",
		Namespace: namespace,
	}, sa); err != nil {
		t.Fatalf("failed to fetch serviceaccount: %v", err)
	}

	if len(sa.OwnerReferences) == 0 {
		t.Fatalf("expected ServiceAccount to carry ownerReference to storagenode CR")
	}
}

func TestStorageNodeReconcileCreatesNamespaceSpecificClusterRoleBindings(t *testing.T) {
	const clusterUUID1 = "cluster-uuid-one"
	const clusterUUID2 = "cluster-uuid-two"

	cluster1 := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster1", Namespace: "cluster1"},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: clusterUUID1},
	}
	cluster2 := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster2", Namespace: "cluster2"},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: clusterUUID2},
	}
	secret1 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-cluster1",
			Namespace: "cluster1",
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID1),
			"secret": []byte("secret1"),
		},
	}
	secret2 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-cluster2",
			Namespace: "cluster2",
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID2),
			"secret": []byte("secret2"),
		},
	}
	sn1 := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-cluster1",
			Namespace:  "cluster1",
			Finalizers: []string{utils.FinalizerStorageNode},
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster1"},
	}
	sn2 := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-cluster2",
			Namespace:  "cluster2",
			Finalizers: []string{utils.FinalizerStorageNode},
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster2"},
	}

	r := newStorageNodeStateTestReconciler(t, sn1, sn2, cluster1, cluster2, secret1, secret2)
	for _, sn := range []*simplyblockv1alpha1.StorageNode{sn2, sn1} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)}); err != nil {
			t.Fatalf("reconcile %s/%s returned error: %v", sn.Namespace, sn.Name, err)
		}
	}

	for _, namespace := range []string{"cluster1", "cluster2"} {
		binding := &rbacv1.ClusterRoleBinding{}
		key := client.ObjectKey{Name: "simplyblock-storage-node-binding-" + namespace}
		if err := r.Get(context.Background(), key, binding); err != nil {
			t.Fatalf("failed to fetch ClusterRoleBinding %s: %v", key.Name, err)
		}
		if len(binding.Subjects) != 1 || binding.Subjects[0].Namespace != namespace {
			t.Fatalf("expected binding %s to target namespace %s, got %#v", key.Name, namespace, binding.Subjects)
		}
	}
}

func TestStorageNodeReconcileMissingInternalIPRequeues(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-missing-ip"
	const clusterUUID = "cluster-uuid-missing-ip"
	const workerName = "node-no-ip"

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: workerName},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-missing-ip",
			Namespace:  namespace,
			Finalizers: []string{utils.FinalizerStorageNode},
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			ClusterName: clusterName,
			WorkerNodes: []string{workerName},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret, node)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue when worker has no internal IP")
	}
}

func TestStorageNodeReconcileUnreachableNodeInfoRequeues(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-unreachable-info"
	const clusterUUID = "cluster-uuid-unreachable-info"
	const workerName = "node-bad-ip"

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: workerName},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{
					Type:    corev1.NodeInternalIP,
					Address: "bad ip",
				},
			},
		},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-unreachable-info",
			Namespace:  namespace,
			Finalizers: []string{utils.FinalizerStorageNode},
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			ClusterName: clusterName,
			WorkerNodes: []string{workerName},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret, node)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue when node info endpoint is unreachable")
	}
}

func TestCheckNodeInfoReachable(t *testing.T) {
	// Use an unroutable test-net address to deterministically exercise error path.
	err := checkNodeInfoReachable(context.Background(), "192.0.2.1", "default", false, false)
	if err == nil {
		t.Fatalf("expected error when node info endpoint is unreachable")
	}
}

func TestCheckNodeInfoReachableTLSMissingCA(t *testing.T) {
	// With TLS enabled and the default CA path (which won't exist in unit tests),
	// the function must surface a build-client error before attempting any I/O.
	err := checkNodeInfoReachable(context.Background(), "192.0.2.1", "default", true, false)
	if err == nil {
		t.Fatalf("expected error when CA bundle is missing")
	}
	if !strings.Contains(err.Error(), "build storage-node TLS client") {
		t.Fatalf("expected TLS client build error, got: %v", err)
	}
}

func TestWaitForNodeInfoReachable(t *testing.T) {
	origCheckFn := waitForNodeInfoReachableCheckFn
	origRetries := waitForNodeInfoReachableMaxRetries
	origDelay := waitForNodeInfoReachableRetryDelay
	t.Cleanup(func() {
		waitForNodeInfoReachableCheckFn = origCheckFn
		waitForNodeInfoReachableMaxRetries = origRetries
		waitForNodeInfoReachableRetryDelay = origDelay
	})

	t.Run("returns nil on first successful check", func(t *testing.T) {
		attempts := 0
		waitForNodeInfoReachableMaxRetries = 3
		waitForNodeInfoReachableRetryDelay = time.Millisecond
		waitForNodeInfoReachableCheckFn = func(context.Context, string, string, bool, bool) error {
			attempts++
			return nil
		}

		if err := waitForNodeInfoReachable(context.Background(), "node-a", "default", false, false); err != nil {
			t.Fatalf("waitForNodeInfoReachable returned error: %v", err)
		}
		if attempts != 1 {
			t.Fatalf("expected one attempt, got %d", attempts)
		}
	})

	t.Run("retries and then succeeds", func(t *testing.T) {
		attempts := 0
		waitForNodeInfoReachableMaxRetries = 4
		waitForNodeInfoReachableRetryDelay = time.Millisecond
		waitForNodeInfoReachableCheckFn = func(context.Context, string, string, bool, bool) error {
			attempts++
			if attempts < 3 {
				return errors.New("temporary failure")
			}
			return nil
		}

		if err := waitForNodeInfoReachable(context.Background(), "node-b", "default", false, false); err != nil {
			t.Fatalf("waitForNodeInfoReachable returned error: %v", err)
		}
		if attempts != 3 {
			t.Fatalf("expected three attempts, got %d", attempts)
		}
	})

	t.Run("returns context cancellation", func(t *testing.T) {
		waitForNodeInfoReachableMaxRetries = 5
		waitForNodeInfoReachableRetryDelay = time.Second
		waitForNodeInfoReachableCheckFn = func(context.Context, string, string, bool, bool) error {
			return errors.New("still down")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := waitForNodeInfoReachable(ctx, "node-c", "default", false, false)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled error, got %v", err)
		}
	})

	t.Run("returns wrapped error after max retries", func(t *testing.T) {
		waitForNodeInfoReachableMaxRetries = 3
		waitForNodeInfoReachableRetryDelay = time.Millisecond
		waitForNodeInfoReachableCheckFn = func(context.Context, string, string, bool, bool) error {
			return errors.New("permanent failure")
		}

		err := waitForNodeInfoReachable(context.Background(), "node-d", "default", false, false)
		if err == nil {
			t.Fatalf("expected timeout error after retries")
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("after %d retries", waitForNodeInfoReachableMaxRetries)) {
			t.Fatalf("unexpected retry error message: %v", err)
		}
		if !strings.Contains(err.Error(), "permanent failure") {
			t.Fatalf("expected wrapped failure message, got: %v", err)
		}
	})
}

func TestPollNodeOnlinePaths(t *testing.T) {
	origActivationDelay := waitForNodeOnlineActivationDelay
	origSleepFn := waitForNodeOnlineSleepFn
	t.Cleanup(func() {
		waitForNodeOnlineActivationDelay = origActivationDelay
		waitForNodeOnlineSleepFn = origSleepFn
	})
	waitForNodeOnlineActivationDelay = 0
	waitForNodeOnlineSleepFn = func(time.Duration) {}

	t.Run("updates node status and returns done when cluster already active", func(t *testing.T) {
		const clusterName = "cluster-a"
		const clusterUUID = "cluster-uuid-online"

		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body: `[
					{
						"id":"node-uuid-1",
						"status":"online",
						"mgmt_ip":"10.0.0.1",
						"health_check":true,
						"hostname":"node-a",
						"online_devices":"nvme0n1",
						"cpu":4,
						"spdk_mem":2147483648,
						"lvols":3,
						"rpc_port":9000,
						"lvol_subsys_port":9001,
						"nvmf_port":9002
					}
				]`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)
		apiClient := webapi.NewClient(mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
			Status: simplyblockv1alpha1.StorageClusterStatus{
				Status:              "active",
				ErasureCodingScheme: "1x0",
			},
		}
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-online", Namespace: "default"},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				ClusterName: clusterName,
				WorkerNodes: []string{"node-a"},
			},
			Status: simplyblockv1alpha1.StorageNodeStatus{
				Nodes: []simplyblockv1alpha1.NodeStatus{
					{Hostname: "node-a", MgmtIp: mgmtIP, Status: "in_creation"},
				},
			},
		}
		r := newStorageNodeStateTestReconciler(t, cluster, sn)

		res, err := r.pollNodeOnline(context.Background(), apiClient, "secret", clusterUUID, mgmtIP, "node-a", 1, sn)
		if err != nil {
			t.Fatalf("pollNodeOnline returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected done result, got requeue: %v", res)
		}
		if len(sn.Status.Nodes) != 1 {
			t.Fatalf("unexpected node status length: %d", len(sn.Status.Nodes))
		}
		got := sn.Status.Nodes[0]
		if got.Status != utils.NodeStatusOnline || got.UUID != "node-uuid-1" {
			t.Fatalf("node status not updated as expected: %#v", got)
		}
	})

	t.Run("appends node status entry when node missing in status list", func(t *testing.T) {
		const clusterName = "cluster-b"
		const clusterUUID = "cluster-uuid-missing-status"

		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body: `[
					{
						"id":"node-uuid-2",
						"status":"online",
						"mgmt_ip":"10.0.0.2",
						"health_check":true,
						"hostname":"node-b",
						"online_devices":"nvme0n2",
						"cpu":8,
						"spdk_mem":4294967296,
						"lvols":1,
						"rpc_port":9100,
						"lvol_subsys_port":9101,
						"nvmf_port":9102
					}
				]`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)
		apiClient := webapi.NewClient(mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
			Status: simplyblockv1alpha1.StorageClusterStatus{
				Status:              "active",
				ErasureCodingScheme: "1x0",
			},
		}
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-missing-status", Namespace: "default"},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				ClusterName: clusterName,
				WorkerNodes: []string{"node-b"},
			},
		}
		r := newStorageNodeStateTestReconciler(t, cluster, sn)

		res, err := r.pollNodeOnline(context.Background(), apiClient, "secret", clusterUUID, "10.0.0.2", "node-b", 1, sn)
		if err != nil {
			t.Fatalf("pollNodeOnline returned unexpected error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected done result, got requeue: %v", res)
		}
		if len(sn.Status.Nodes) != 1 {
			t.Fatalf("expected 1 status entry, got %d", len(sn.Status.Nodes))
		}
		got := sn.Status.Nodes[0]
		if got.Status != statusOnline || got.UUID != "node-uuid-2" || got.Hostname != "node-b" {
			t.Fatalf("unexpected appended node status: %#v", got)
		}
	})

	t.Run("returns RequeueAfter when node not yet online and within timeout window", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-not-yet-online"
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/",
			webapimock.RouteResponse{
				Status:  http.StatusOK,
				Body:    `[]`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)

		postedAt := metav1.Now()
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-not-yet-online", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
			Status: simplyblockv1alpha1.StorageNodeStatus{
				Nodes: []simplyblockv1alpha1.NodeStatus{
					{Hostname: "node-a", MgmtIp: mgmtIP, Status: "in_creation", PostedAt: &postedAt},
				},
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		res, err := r.pollNodeOnline(context.Background(), webapi.NewClient(mock.URL()), "secret", clusterUUID, mgmtIP, "node-a", 1, sn)
		if err != nil {
			t.Fatalf("pollNodeOnline returned unexpected error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected RequeueAfter, got done result")
		}
	})
}

func TestPollNodeOnlineErrorAndTimeoutPaths(t *testing.T) {
	origActivationDelay := waitForNodeOnlineActivationDelay
	origSleepFn := waitForNodeOnlineSleepFn
	t.Cleanup(func() {
		waitForNodeOnlineActivationDelay = origActivationDelay
		waitForNodeOnlineSleepFn = origSleepFn
	})
	waitForNodeOnlineActivationDelay = 0
	waitForNodeOnlineSleepFn = func(time.Duration) {}

	t.Run("returns error on invalid storage-node payload", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-wfno-invalid-json"
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body:   `{`,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
			},
		)

		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-wfno-invalid-json", Namespace: "default"},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				ClusterName: "cluster-a",
			},
			Status: simplyblockv1alpha1.StorageNodeStatus{
				Nodes: []simplyblockv1alpha1.NodeStatus{
					{Hostname: "node-a", MgmtIp: mgmtIP, Status: "in_creation"},
				},
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		_, err := r.pollNodeOnline(context.Background(), webapi.NewClient(mock.URL()), "secret", clusterUUID, mgmtIP, "node-a", 1, sn)
		if err == nil {
			t.Fatalf("expected unmarshal error for invalid payload")
		}
		if !strings.Contains(err.Error(), "failed to unmarshal") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("returns cluster-not-found when activation precheck cannot resolve cluster CR", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-wfno-cluster-missing"
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body: `[
					{
						"id":"node-uuid-3",
						"status":"online",
						"mgmt_ip":"10.0.0.3",
						"health_check":true,
						"hostname":"node-c",
						"online_devices":"nvme1n1",
						"cpu":4,
						"spdk_mem":2147483648,
						"lvols":1,
						"rpc_port":9200,
						"lvol_subsys_port":9201,
						"nvmf_port":9202
					}
				]`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)

		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-wfno-cluster-missing", Namespace: "default"},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				ClusterName: "cluster-missing",
			},
			Status: simplyblockv1alpha1.StorageNodeStatus{
				Nodes: []simplyblockv1alpha1.NodeStatus{
					{Hostname: "node-c", MgmtIp: "10.0.0.3", Status: "in_creation"},
				},
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		_, err := r.pollNodeOnline(context.Background(), webapi.NewClient(mock.URL()), "secret", clusterUUID, "10.0.0.3", "node-c", 1, sn)
		if err == nil {
			t.Fatalf("expected cluster resolution error")
		}
		if !strings.Contains(err.Error(), "cluster not found yet") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("writes timeout node status when PostedAt is expired", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-wfno-timeout"
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body:   `[]`,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
			},
		)

		expiredAt := metav1.NewTime(time.Now().Add(-2 * time.Hour))
		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-wfno-timeout", Namespace: "default"},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				ClusterName: "cluster-a",
			},
			Status: simplyblockv1alpha1.StorageNodeStatus{
				Nodes: []simplyblockv1alpha1.NodeStatus{
					{Hostname: "node-timeout", MgmtIp: "10.0.0.4", Status: "in_creation", PostedAt: &expiredAt},
				},
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		res, err := r.pollNodeOnline(context.Background(), webapi.NewClient(mock.URL()), "secret", clusterUUID, "10.0.0.4", "node-timeout", 1, sn)
		if err != nil {
			t.Fatalf("expected no error on timeout, got: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected done result after timeout, got requeue: %v", res)
		}
		if len(sn.Status.Nodes) != 1 {
			t.Fatalf("expected timeout status node entry, got %d", len(sn.Status.Nodes))
		}
		if sn.Status.Nodes[0].Hostname != "node-timeout" || sn.Status.Nodes[0].Status != "timeout" {
			t.Fatalf("unexpected timeout node status: %#v", sn.Status.Nodes[0])
		}
	})
}

func TestWaitForActionCompletionRetryBehavior(t *testing.T) {
	origRetries := waitForActionCompletionRetries
	origWait := waitForActionCompletionWaitInterval
	origSleepFn := waitForActionCompletionSleepFn
	t.Cleanup(func() {
		waitForActionCompletionRetries = origRetries
		waitForActionCompletionWaitInterval = origWait
		waitForActionCompletionSleepFn = origSleepFn
	})

	waitForActionCompletionRetries = 4
	waitForActionCompletionWaitInterval = 0
	waitForActionCompletionSleepFn = func(time.Duration) {}

	t.Run("returns terminal error when target status never reached", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(utils.NodeStatusResponse{
				Status: "creating",
			})
		}))
		defer srv.Close()

		r := &StorageNodeReconciler{}
		err := r.waitForActionCompletion(
			context.Background(),
			webapi.NewClient(srv.URL),
			"cluster-a",
			"secret",
			"node-a",
			"restart",
		)
		if err == nil {
			t.Fatalf("expected terminal status error")
		}
		if !strings.Contains(err.Error(), "did not reach expected status") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("recovers from transient API and payload errors before success", func(t *testing.T) {
		attempts := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts++
			switch attempts {
			case 1:
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"temporary"}`))
			case 2:
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{`))
			default:
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(utils.NodeStatusResponse{
					Status: statusOnline,
				})
			}
		}))
		defer srv.Close()

		r := &StorageNodeReconciler{}
		err := r.waitForActionCompletion(
			context.Background(),
			webapi.NewClient(srv.URL),
			"cluster-b",
			"secret",
			"node-b",
			"restart",
		)
		if err != nil {
			t.Fatalf("expected eventual success, got error: %v", err)
		}
		if attempts != 3 {
			t.Fatalf("expected 3 attempts, got %d", attempts)
		}
	})
}

func TestPerformNodeActionRemoveHappyPath(t *testing.T) {
	const clusterUUID = "cluster-uuid-remove"
	const nodeUUID = "node-uuid-remove"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	mock.Register(
		http.MethodDelete,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID,
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID,
		webapimock.RouteResponse{Status: http.StatusNotFound},
	)
	apiClient := webapi.NewClient(mock.URL())

	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-remove", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			Action:   "remove",
			NodeUUID: nodeUUID,
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn)

	if err := r.performNodeAction(context.Background(), apiClient, clusterUUID, "secret", sn); err != nil {
		t.Fatalf("performNodeAction(remove) returned error: %v", err)
	}
}

func TestPerformNodeActionRestartWorkerNodeLabelFailure(t *testing.T) {
	const clusterUUID = "cluster-uuid-restart-label-fail"
	const nodeUUID = "node-uuid-restart-label-fail"

	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-restart-label-fail", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			Action:     "restart",
			NodeUUID:   nodeUUID,
			WorkerNode: "missing-node",
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn)

	err := r.performNodeAction(
		context.Background(),
		webapi.NewClient("http://127.0.0.1:1"),
		clusterUUID,
		"secret",
		sn,
	)
	if err == nil {
		t.Fatalf("expected restart action to fail when worker node lookup fails")
	}
	if !strings.Contains(err.Error(), "failed to label worker node") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPerformNodeActionRestartWorkerNodeReachabilityCanceled(t *testing.T) {
	const clusterUUID = "cluster-uuid-restart-cancel"
	const nodeUUID = "node-uuid-restart-cancel"
	const workerNode = "node-cancel"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: workerNode},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.99"},
			},
		},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-restart-cancel", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			Action:      "restart",
			NodeUUID:    nodeUUID,
			WorkerNode:  workerNode,
			ClusterName: "cluster-a",
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn, node)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := r.performNodeAction(
		ctx,
		webapi.NewClient("http://127.0.0.1:1"),
		clusterUUID,
		"secret",
		sn,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation error from restart reachability path, got %v", err)
	}
}

func TestPerformNodeActionAPIFailure(t *testing.T) {
	t.Run("restart returns API failure on non-2xx", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-restart-api-fail"
		const nodeUUID = "node-uuid-restart-api-fail"

		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodPost,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID+"/restart",
			webapimock.RouteResponse{
				Status: http.StatusInternalServerError,
				Body:   `{"error":"boom"}`,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
			},
		)

		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-restart-api-fail", Namespace: "default"},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				Action:   "restart",
				NodeUUID: nodeUUID,
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)
		err := r.performNodeAction(context.Background(), webapi.NewClient(mock.URL()), clusterUUID, "secret", sn)
		if err == nil {
			t.Fatalf("expected restart API failure")
		}
		if !strings.Contains(err.Error(), "action API failed") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("default action endpoint returns API failure on non-2xx", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-default-api-fail"
		const nodeUUID = "node-uuid-default-api-fail"

		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodPost,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID+"/suspend?force=true",
			webapimock.RouteResponse{
				Status: http.StatusBadGateway,
				Body:   `{"error":"upstream failed"}`,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
			},
		)

		sn := &simplyblockv1alpha1.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-default-api-fail", Namespace: "default"},
			Spec: simplyblockv1alpha1.StorageNodeSpec{
				Action:   "suspend",
				NodeUUID: nodeUUID,
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)
		err := r.performNodeAction(context.Background(), webapi.NewClient(mock.URL()), clusterUUID, "secret", sn)
		if err == nil {
			t.Fatalf("expected default-action API failure")
		}
		if !strings.Contains(err.Error(), "action API failed") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestPerformNodeActionDefaultActionSuccess(t *testing.T) {
	const clusterUUID = "cluster-uuid-default-success"
	const nodeUUID = "node-uuid-default-success"

	origDelay := performNodeActionPostTriggerDelay
	origSleep := performNodeActionSleepFn
	t.Cleanup(func() {
		performNodeActionPostTriggerDelay = origDelay
		performNodeActionSleepFn = origSleep
	})
	performNodeActionPostTriggerDelay = 0
	performNodeActionSleepFn = func(time.Duration) {}

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	mock.Register(
		http.MethodPost,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID+"/suspend",
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body:   `{}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)
	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID,
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body:   `{"status":"suspended"}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)

	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-default-success", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			Action:   "suspend",
			NodeUUID: nodeUUID,
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn)
	if err := r.performNodeAction(context.Background(), webapi.NewClient(mock.URL()), clusterUUID, "secret", sn); err != nil {
		t.Fatalf("performNodeAction(default suspend) returned error: %v", err)
	}
}

func TestHandleNodeActionTransitionsToSuccess(t *testing.T) {
	const clusterUUID = "cluster-uuid-action-success"
	const nodeUUID = "node-uuid-action-success"

	origDelay := performNodeActionPostTriggerDelay
	origSleep := performNodeActionSleepFn
	t.Cleanup(func() {
		performNodeActionPostTriggerDelay = origDelay
		performNodeActionSleepFn = origSleep
	})
	performNodeActionPostTriggerDelay = 0
	performNodeActionSleepFn = func(time.Duration) {}

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	mock.Register(
		http.MethodDelete,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID,
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID,
		webapimock.RouteResponse{Status: http.StatusNotFound},
	)

	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-action-success", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			Action:   "remove",
			NodeUUID: nodeUUID,
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn)

	if err := r.handleNodeAction(context.Background(), webapi.NewClient(mock.URL()), sn, clusterUUID, "secret"); err != nil {
		t.Fatalf("handleNodeAction returned error: %v", err)
	}

	current := &simplyblockv1alpha1.StorageNode{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), current); err != nil {
		t.Fatalf("failed to fetch storagenode: %v", err)
	}
	if current.Status.ActionStatus == nil {
		t.Fatalf("expected action status to be set")
	}
	if current.Status.ActionStatus.State != utils.ActionStateSuccess {
		t.Fatalf("expected success action state, got %q", current.Status.ActionStatus.State)
	}
}

// testOperatorNamespace is the namespace the test reconciler pretends to run in.
// It must match the namespace of the seeded singleton ControlPlane CR below.
const testOperatorNamespace = "default"

func newStorageNodeStateTestReconciler(
	t *testing.T,
	objects ...client.Object,
) *StorageNodeReconciler {
	t.Helper()

	scheme := newTestScheme(
		t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
		appsv1.AddToScheme,
		rbacv1.AddToScheme,
		discoveryv1.AddToScheme,
	)

	// Mirror real-cluster state: the Helm chart always creates the singleton
	// ControlPlane CR before any StorageNode CR is reconciled.
	singleton := &simplyblockv1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SingletonControlPlaneName,
			Namespace: testOperatorNamespace,
		},
		Spec: simplyblockv1alpha1.ControlPlaneSpec{
			Image: "test-image:latest",
		},
	}
	allObjects := append([]client.Object{singleton}, objects...)

	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.StorageNode{},
		&simplyblockv1alpha1.StorageCluster{},
		&simplyblockv1alpha1.ControlPlane{},
		&appsv1.DaemonSet{},
	}, allObjects...)

	return &StorageNodeReconciler{
		Client:    cl,
		Scheme:    scheme,
		Namespace: testOperatorNamespace,
	}
}

func TestReconcileSpdkProxyService(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn", Namespace: "ns", UID: "sn-uid"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
	}

	r := newStorageNodeStateTestReconciler(t, sn)
	r.TLSEnabled = true
	r.TLSProvider = utils.TLSProviderOpenShift

	if err := r.reconcileSpdkProxyService(context.Background(), sn); err != nil {
		t.Fatalf("reconcileSpdkProxyService: %v", err)
	}

	var svc corev1.Service
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "simplyblock-spdk-proxy"}, &svc); err != nil {
		t.Fatalf("expected simplyblock-spdk-proxy Service to be created: %v", err)
	}
	if svc.Spec.ClusterIP != "None" {
		t.Fatalf("expected headless Service, got ClusterIP=%q", svc.Spec.ClusterIP)
	}
	if len(svc.Spec.Ports) != 0 {
		t.Fatalf("expected no ports on Service, got %#v", svc.Spec.Ports)
	}
	if got := svc.Annotations["service.beta.openshift.io/serving-cert-secret-name"]; got != "simplyblock-spdk-proxy-tls" {
		t.Fatalf("missing/incorrect serving-cert annotation: %q", got)
	}
	if len(svc.OwnerReferences) != 1 || svc.OwnerReferences[0].UID != "sn-uid" {
		t.Fatalf("expected owner reference to StorageNode, got %#v", svc.OwnerReferences)
	}

	// Second pass with a simulated ClusterIP already assigned must preserve it.
	svc.Spec.ClusterIP = "None"
	if err := r.reconcileSpdkProxyService(context.Background(), sn); err != nil {
		t.Fatalf("second reconcileSpdkProxyService: %v", err)
	}
}

func TestReconcileServicesAndServingCertificatesForCertManagerProvider(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn", Namespace: "ns", UID: "sn-uid"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
	}

	r := newStorageNodeStateTestReconciler(t, sn)
	r.TLSEnabled = true
	r.TLSProvider = utils.TLSProviderCertManager

	if err := r.reconcileService(context.Background(), sn); err != nil {
		t.Fatalf("reconcileService: %v", err)
	}
	if err := r.reconcileSpdkProxyService(context.Background(), sn); err != nil {
		t.Fatalf("reconcileSpdkProxyService: %v", err)
	}
	if err := r.reconcileServingCertificates(context.Background(), sn); err != nil {
		t.Fatalf("reconcileServingCertificates: %v", err)
	}

	for _, serviceName := range []string{"simplyblock-storage-node-api", "simplyblock-spdk-proxy"} {
		var svc corev1.Service
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: serviceName}, &svc); err != nil {
			t.Fatalf("expected Service %s to be created: %v", serviceName, err)
		}
		if got := svc.Annotations[utils.OpenShiftServingCertAnnotation]; got != "" {
			t.Fatalf("unexpected OpenShift serving-cert annotation on %s: %q", serviceName, got)
		}
	}

	for serviceName, secretName := range map[string]string{
		"simplyblock-storage-node-api": "simplyblock-storage-node-api-tls",
		"simplyblock-spdk-proxy":       "simplyblock-spdk-proxy-tls",
	} {
		cert := &unstructured.Unstructured{}
		cert.SetAPIVersion("cert-manager.io/v1")
		cert.SetKind("Certificate")
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: serviceName}, cert); err != nil {
			t.Fatalf("expected Certificate %s to be created: %v", serviceName, err)
		}

		gotSecret, found, err := unstructured.NestedString(cert.Object, "spec", "secretName")
		if err != nil || !found {
			t.Fatalf("expected secretName on Certificate %s, err=%v found=%v", serviceName, err, found)
		}
		if gotSecret != secretName {
			t.Fatalf("Certificate %s secretName = %q, want %q", serviceName, gotSecret, secretName)
		}

		gotIssuer, found, err := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
		if err != nil || !found {
			t.Fatalf("expected issuerRef.name on Certificate %s, err=%v found=%v", serviceName, err, found)
		}
		if gotIssuer != utils.CertManagerClusterIssuerName {
			t.Fatalf("Certificate %s issuerRef.name = %q, want %q", serviceName, gotIssuer, utils.CertManagerClusterIssuerName)
		}

		dnsNames, found, err := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
		if err != nil || !found {
			t.Fatalf("expected dnsNames on Certificate %s, err=%v found=%v", serviceName, err, found)
		}
		if !contains(dnsNames, serviceName) || !contains(dnsNames, serviceName+".ns.svc.cluster.local") {
			t.Fatalf("Certificate %s dnsNames = %#v", serviceName, dnsNames)
		}
	}
}

func TestReconcileSpdkProxyEndpointSlices(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn", Namespace: "ns", UID: "sn-uid"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
	}

	podReady := func(name, node, ip, rpcPort string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "ns",
				Labels:    map[string]string{"role": "simplyblock-storage-node"},
			},
			Spec: corev1.PodSpec{
				NodeName: node,
				Containers: []corev1.Container{
					{
						Name: "spdk-proxy-container",
						Env:  []corev1.EnvVar{{Name: "RPC_PORT", Value: rpcPort}},
					},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: ip,
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "spdk-proxy-container", Ready: true},
				},
			},
		}
	}

	pod1 := podReady("snode-spdk-pod-9001-cid", "node-a", mgmtIP, "9001")
	pod2 := podReady("snode-spdk-pod-9002-cid", "node-a", mgmtIP, "9002")
	pod3 := podReady("snode-spdk-pod-9001-cid-b", "node-b", "10.0.0.2", "9001")

	// wrong label — must be ignored
	ignored := podReady("other", "node-c", "10.0.0.3", "9001")
	ignored.Labels = map[string]string{"role": "other"}

	// not ready — must be ignored
	notReady := podReady("not-ready", "node-a", mgmtIP, "9003")
	notReady.Status.ContainerStatuses[0].Ready = false

	r := newStorageNodeStateTestReconciler(t, sn, pod1, pod2, pod3, ignored, notReady)

	ctx := context.Background()
	if err := r.reconcileSpdkProxyEndpointSlices(ctx, sn); err != nil {
		t.Fatalf("reconcileSpdkProxyEndpointSlices: %v", err)
	}

	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices,
		client.InNamespace("ns"),
		client.MatchingLabels{"kubernetes.io/service-name": "simplyblock-spdk-proxy"},
	); err != nil {
		t.Fatalf("list slices: %v", err)
	}
	if len(slices.Items) != 2 {
		t.Fatalf("expected 2 EndpointSlices, got %d", len(slices.Items))
	}

	byName := map[string]discoveryv1.EndpointSlice{}
	for _, s := range slices.Items {
		byName[s.Name] = s
	}

	slice9001, ok := byName["spdk-proxy-endpoints-9001"]
	if !ok {
		t.Fatalf("missing slice spdk-proxy-endpoints-9001; got %v", sliceNames(slices.Items))
	}
	if len(slice9001.Endpoints) != 2 {
		t.Fatalf("slice 9001: expected 2 endpoints, got %d", len(slice9001.Endpoints))
	}
	gotHostnames := map[string]string{}
	for _, ep := range slice9001.Endpoints {
		if ep.Hostname == nil || len(ep.Addresses) != 1 {
			t.Fatalf("slice 9001: malformed endpoint %#v", ep)
		}
		gotHostnames[*ep.Hostname] = ep.Addresses[0]
	}
	if gotHostnames["node-a"] != mgmtIP ||
		gotHostnames["node-b"] != "10.0.0.2" {
		t.Fatalf("slice 9001: unexpected hostname/address map %#v", gotHostnames)
	}
	if len(slice9001.Ports) != 1 || slice9001.Ports[0].Port == nil || *slice9001.Ports[0].Port != 9001 {
		t.Fatalf("slice 9001: expected port 9001, got %#v", slice9001.Ports)
	}
	if !metav1.IsControlledBy(&slice9001, sn) {
		t.Fatalf("slice 9001: expected owner reference to StorageNode")
	}

	slice9002 := byName["spdk-proxy-endpoints-9002"]
	if len(slice9002.Endpoints) != 1 || *slice9002.Endpoints[0].Hostname != "node-a" {
		t.Fatalf("slice 9002: unexpected endpoints %#v", slice9002.Endpoints)
	}

	// Delete pod2 (the only pod on port 9002) and reconcile again — the stale
	// slice should be removed.
	if err := r.Delete(ctx, pod2); err != nil {
		t.Fatalf("delete pod2: %v", err)
	}
	if err := r.reconcileSpdkProxyEndpointSlices(ctx, sn); err != nil {
		t.Fatalf("second reconcileSpdkProxyEndpointSlices: %v", err)
	}

	slices = discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, &slices,
		client.InNamespace("ns"),
		client.MatchingLabels{"kubernetes.io/service-name": "simplyblock-spdk-proxy"},
	); err != nil {
		t.Fatalf("list slices after delete: %v", err)
	}
	if len(slices.Items) != 1 || slices.Items[0].Name != "spdk-proxy-endpoints-9001" {
		t.Fatalf("expected only spdk-proxy-endpoints-9001 after pod2 deletion, got %v", sliceNames(slices.Items))
	}
}

func TestReconcileSpdkProxyEndpointSlices_DuplicateFirstSegment(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn", Namespace: "ns", UID: "sn-uid"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
	}

	podReady := func(name, node, ip, rpcPort string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "ns",
				Labels:    map[string]string{"role": "simplyblock-storage-node"},
			},
			Spec: corev1.PodSpec{
				NodeName: node,
				Containers: []corev1.Container{
					{
						Name: "spdk-proxy-container",
						Env:  []corev1.EnvVar{{Name: "RPC_PORT", Value: rpcPort}},
					},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: ip,
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "spdk-proxy-container", Ready: true},
				},
			},
		}
	}

	// Two distinct nodes whose first DNS label collides.
	pod1 := podReady("snode-spdk-pod-9001-a", "worker.us-east-1.local", "10.0.0.1", "9001")
	pod2 := podReady("snode-spdk-pod-9001-b", "worker.eu-west-1.local", "10.0.0.2", "9001")

	r := newStorageNodeStateTestReconciler(t, sn, pod1, pod2)

	ctx := context.Background()
	err := r.reconcileSpdkProxyEndpointSlices(ctx, sn)
	if err == nil {
		t.Fatalf("expected collision error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "worker.us-east-1.local") || !strings.Contains(msg, "worker.eu-west-1.local") {
		t.Fatalf("expected error to name both colliding nodes, got %q", msg)
	}

	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices,
		client.InNamespace("ns"),
		client.MatchingLabels{"kubernetes.io/service-name": "simplyblock-spdk-proxy"},
	); err != nil {
		t.Fatalf("list slices: %v", err)
	}
	if len(slices.Items) != 0 {
		t.Fatalf("expected no slices to be created on collision, got %v", sliceNames(slices.Items))
	}
}

func TestExtractSpdkProxyRpcPort_FallbackToPodName(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "snode-spdk-pod-9004-mycluster"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "spdk-proxy-container"}, // no RPC_PORT env
			},
		},
	}
	got, ok := extractSpdkProxyRpcPort(pod)
	if !ok || got != 9004 {
		t.Fatalf("expected (9004,true) from pod-name fallback, got (%d,%v)", got, ok)
	}
}

func sliceNames(items []discoveryv1.EndpointSlice) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, s.Name)
	}
	return out
}

func TestStorageNodeDaemonSetTLSSecretRevisionAnnotation(t *testing.T) {
	const (
		ns          = "default"
		clusterName = "cluster-a"
		dsName      = "simplyblock-storage-node-ds-cluster-a"
	)

	cases := []struct {
		name       string
		tlsEnabled bool
		seedSecret bool
		secretRV   string
		wantValue  string
		wantSet    bool
	}{
		{
			name:       "tls enabled with secret stamps annotation",
			tlsEnabled: true,
			seedSecret: true,
			secretRV:   "12345",
			wantValue:  "12345",
			wantSet:    true,
		},
		{
			name:       "tls enabled but secret missing leaves annotation unset",
			tlsEnabled: true,
			seedSecret: false,
			wantSet:    false,
		},
		{
			name:       "tls disabled leaves annotation unset even if secret exists",
			tlsEnabled: false,
			seedSecret: true,
			secretRV:   "67890",
			wantSet:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sn := &simplyblockv1alpha1.StorageNode{
				ObjectMeta: metav1.ObjectMeta{Name: "sn-ds-rv", Namespace: ns, UID: "uid-rv"},
				Spec:       simplyblockv1alpha1.StorageNodeSpec{ClusterName: clusterName},
			}
			objs := []client.Object{sn}
			if tc.seedSecret {
				objs = append(objs, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:            utils.SecretNameStorageNodeAPITLS,
						Namespace:       ns,
						ResourceVersion: tc.secretRV,
					},
				})
			}
			r := newStorageNodeStateTestReconciler(t, objs...)
			r.TLSEnabled = tc.tlsEnabled
			r.TLSProvider = utils.TLSProviderCertManager

			if err := r.reconcileDaemonSet(context.Background(), sn); err != nil {
				t.Fatalf("reconcileDaemonSet returned error: %v", err)
			}

			var ds appsv1.DaemonSet
			if err := r.Get(context.Background(), client.ObjectKey{Name: dsName, Namespace: ns}, &ds); err != nil {
				t.Fatalf("failed to fetch daemonset: %v", err)
			}

			got, ok := ds.Spec.Template.Annotations[utils.AnnotationTLSSecretRevision]
			switch {
			case tc.wantSet && !ok:
				t.Fatalf("expected pod-template annotation %q to be set", utils.AnnotationTLSSecretRevision)
			case tc.wantSet && got != tc.wantValue:
				t.Fatalf("annotation value: want %q, got %q", tc.wantValue, got)
			case !tc.wantSet && ok:
				t.Fatalf("expected pod-template annotation %q to be unset, got %q", utils.AnnotationTLSSecretRevision, got)
			}
		})
	}
}

func TestStorageNodeDaemonSetReconcileRollsOnTLSSecretRevisionChange(t *testing.T) {
	const ns = "default"
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-ds-roll", Namespace: ns, UID: "uid-roll"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            utils.SecretNameStorageNodeAPITLS,
			Namespace:       ns,
			ResourceVersion: "1",
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn, secret)
	r.TLSEnabled = true
	r.TLSProvider = utils.TLSProviderCertManager

	if err := r.reconcileDaemonSet(context.Background(), sn); err != nil {
		t.Fatalf("first reconcileDaemonSet: %v", err)
	}

	dsKey := client.ObjectKey{Name: "simplyblock-storage-node-ds-cluster-a", Namespace: ns}
	var first appsv1.DaemonSet
	if err := r.Get(context.Background(), dsKey, &first); err != nil {
		t.Fatalf("fetch first daemonset: %v", err)
	}

	// Simulate cert-manager rotating the Secret: any Update bumps
	// metadata.resourceVersion via the fake client's bookkeeping.
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: utils.SecretNameStorageNodeAPITLS}, secret); err != nil {
		t.Fatalf("refetch secret: %v", err)
	}
	secret.Data = map[string][]byte{"tls.crt": []byte("rotated")}
	if err := r.Update(context.Background(), secret); err != nil {
		t.Fatalf("rotate secret: %v", err)
	}

	if err := r.reconcileDaemonSet(context.Background(), sn); err != nil {
		t.Fatalf("second reconcileDaemonSet: %v", err)
	}

	var second appsv1.DaemonSet
	if err := r.Get(context.Background(), dsKey, &second); err != nil {
		t.Fatalf("fetch second daemonset: %v", err)
	}

	firstRV := first.Spec.Template.Annotations[utils.AnnotationTLSSecretRevision]
	secondRV := second.Spec.Template.Annotations[utils.AnnotationTLSSecretRevision]
	if firstRV == "" || secondRV == "" {
		t.Fatalf("expected pod-template annotation set in both passes, got first=%q second=%q", firstRV, secondRV)
	}
	if firstRV == secondRV {
		t.Fatalf("expected pod-template annotation to change after Secret rotation, both still %q", firstRV)
	}
}

func TestStorageNodeDaemonSetSBTLSServeEnv(t *testing.T) {
	cases := []struct {
		name            string
		tlsEnabled      bool
		tlsProvider     string
		wantServe       string
		wantServeSet    bool
		wantProvider    string
		wantProviderSet bool
	}{
		{
			name:            "tls enabled with cert-manager",
			tlsEnabled:      true,
			tlsProvider:     utils.TLSProviderCertManager,
			wantServe:       "true",
			wantServeSet:    true,
			wantProvider:    utils.TLSProviderCertManager,
			wantProviderSet: true,
		},
		{
			name:            "tls enabled with OpenShift",
			tlsEnabled:      true,
			tlsProvider:     utils.TLSProviderOpenShift,
			wantServe:       "true",
			wantServeSet:    true,
			wantProvider:    utils.TLSProviderOpenShift,
			wantProviderSet: true,
		},
		{
			name:         "tls disabled omits TLS env vars",
			tlsEnabled:   false,
			tlsProvider:  utils.TLSProviderCertManager,
			wantServeSet: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sn := &simplyblockv1alpha1.StorageNode{
				ObjectMeta: metav1.ObjectMeta{Name: "sn-env", Namespace: "default", UID: "uid-env"},
				Spec:       simplyblockv1alpha1.StorageNodeSpec{ClusterName: "cluster-a"},
			}
			r := newStorageNodeStateTestReconciler(t, sn)
			r.TLSEnabled = tc.tlsEnabled
			r.TLSProvider = tc.tlsProvider

			if err := r.reconcileDaemonSet(context.Background(), sn); err != nil {
				t.Fatalf("reconcileDaemonSet returned error: %v", err)
			}

			var ds appsv1.DaemonSet
			if err := r.Get(context.Background(), client.ObjectKey{Name: "simplyblock-storage-node-ds-cluster-a", Namespace: "default"}, &ds); err != nil {
				t.Fatalf("failed to fetch daemonset: %v", err)
			}
			if len(ds.Spec.Template.Spec.Containers) != 1 {
				t.Fatalf("expected single main container, got %d", len(ds.Spec.Template.Spec.Containers))
			}

			envByName := map[string]string{}
			envSeen := map[string]bool{}
			for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
				envByName[e.Name] = e.Value
				envSeen[e.Name] = true
			}

			switch {
			case tc.wantServeSet && !envSeen["SB_TLS_SERVE"]:
				t.Fatalf("expected SB_TLS_SERVE env var to be set on main container")
			case tc.wantServeSet && envByName["SB_TLS_SERVE"] != tc.wantServe:
				t.Fatalf("SB_TLS_SERVE: want %q, got %q", tc.wantServe, envByName["SB_TLS_SERVE"])
			case !tc.wantServeSet && envSeen["SB_TLS_SERVE"]:
				t.Fatalf("expected SB_TLS_SERVE env var to be absent, got %q", envByName["SB_TLS_SERVE"])
			}

			switch {
			case tc.wantProviderSet && !envSeen["SB_TLS_PROVIDER"]:
				t.Fatalf("expected SB_TLS_PROVIDER env var to be set on main container")
			case tc.wantProviderSet && envByName["SB_TLS_PROVIDER"] != tc.wantProvider:
				t.Fatalf("SB_TLS_PROVIDER: want %q, got %q", tc.wantProvider, envByName["SB_TLS_PROVIDER"])
			case !tc.wantProviderSet && envSeen["SB_TLS_PROVIDER"]:
				t.Fatalf("expected SB_TLS_PROVIDER env var to be absent, got %q", envByName["SB_TLS_PROVIDER"])
			}
		})
	}
}

func TestIsStorageNodeTLSSecretPredicate(t *testing.T) {
	cases := []struct {
		name string
		obj  client.Object
		want bool
	}{
		{
			name: "matches storage-node-api TLS secret",
			obj:  &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: utils.SecretNameStorageNodeAPITLS}},
			want: true,
		},
		{
			name: "ignores spdk-proxy TLS secret",
			obj:  &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: utils.SecretNameSpdkProxyTLS}},
			want: false,
		},
		{
			name: "ignores unrelated secret",
			obj:  &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "some-other-secret"}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStorageNodeTLSSecret(tc.obj); got != tc.want {
				t.Fatalf("isStorageNodeTLSSecret(%q) = %v, want %v", tc.obj.GetName(), got, tc.want)
			}
		})
	}
}

func TestTLSSecretToStorageNodeRequestsEnqueuesAllInNamespace(t *testing.T) {
	const ns = "ns"
	snA := &simplyblockv1alpha1.StorageNode{ObjectMeta: metav1.ObjectMeta{Name: "sn-a", Namespace: ns}}
	snB := &simplyblockv1alpha1.StorageNode{ObjectMeta: metav1.ObjectMeta{Name: "sn-b", Namespace: ns}}
	otherNS := &simplyblockv1alpha1.StorageNode{ObjectMeta: metav1.ObjectMeta{Name: "sn-c", Namespace: "other"}}

	r := newStorageNodeStateTestReconciler(t, snA, snB, otherNS)

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      utils.SecretNameStorageNodeAPITLS,
		Namespace: ns,
	}}
	reqs := r.tlsSecretToStorageNodeRequests(context.Background(), secret)

	got := make(map[string]bool, len(reqs))
	for _, req := range reqs {
		got[req.Namespace+"/"+req.Name] = true
	}

	want := map[string]bool{ns + "/sn-a": true, ns + "/sn-b": true}
	if len(got) != len(want) {
		t.Fatalf("expected %d requests, got %d (%v)", len(want), len(got), got)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("missing reconcile request for %q", k)
		}
	}
	if got[ns+"/sn-c"] || got["other/sn-c"] {
		t.Fatalf("did not expect cross-namespace StorageNode to be enqueued: %v", got)
	}
}
