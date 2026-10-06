// The NUMA device plugin a storage plane runs beside its storage nodes.
//
// The plugin advertises numa-align/numa-<socket> on each storage worker, and the
// control plane's storage pods request it so that a pod lands next to the memory
// and devices of one socket. The OpenShift node bootstrap waits for the plugin
// in kube-system by label, so the plugin lives there and nowhere else.
//
// kube-system is not the cluster's namespace, which decides the rest. An object
// there cannot carry a controller reference to a StorageCluster, and every
// cluster of the installation shares the one DaemonSet, so ownership is a label
// and the DaemonSet goes when the last cluster does.

package node

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func numaDaemonSet(t *testing.T, r *StorageNodeWorkloadReconciler) (*appsv1.DaemonSet, error) {
	t.Helper()
	var ds appsv1.DaemonSet
	err := r.Get(context.Background(),
		client.ObjectKey{Namespace: "kube-system", Name: "simplyblock-numa-resource-plugin"}, &ds)
	return &ds, err
}

func mustHaveNUMAPlugin(t *testing.T, r *StorageNodeWorkloadReconciler) *appsv1.DaemonSet {
	t.Helper()
	ds, err := numaDaemonSet(t, r)
	if err != nil {
		t.Fatalf("the NUMA plugin DaemonSet does not exist: %v", err)
	}
	return ds
}

func anotherCluster() *simplyblockv1alpha2.StorageCluster {
	cluster := aSizedCluster()
	cluster.Name = "a-second-numa-cluster"
	return cluster
}

// A cluster brings the plugin: the DaemonSet and the account it runs as, in
// kube-system, selecting the storage plane.
func TestAClusterGetsTheNUMAPlugin(t *testing.T) {
	r := aWorkloadReconciler(t, aSizedCluster())

	if err := r.reconcileNUMAPlugin(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("reconcile the plugin: %v", err)
	}

	ds := mustHaveNUMAPlugin(t, r)
	var account corev1.ServiceAccount
	if err := r.Get(context.Background(),
		client.ObjectKey{Namespace: "kube-system", Name: "simplyblock-numa-resource-plugin"}, &account); err != nil {
		t.Fatalf("the plugin's ServiceAccount does not exist: %v", err)
	}
	if ds.Spec.Template.Spec.ServiceAccountName != account.Name {
		t.Errorf("the DaemonSet runs as %q, want %q", ds.Spec.Template.Spec.ServiceAccountName, account.Name)
	}

	// The OpenShift bootstrap finds the pod by this label, in this namespace.
	if ds.Spec.Template.Labels["app"] != "simplyblock-numa-resource-plugin" {
		t.Errorf("pod labels %v lack the app label the node bootstrap waits on", ds.Spec.Template.Labels)
	}

	terms := ds.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 ||
		terms[0].MatchExpressions[0].Key != "io.simplyblock.storagenodeset" ||
		terms[0].MatchExpressions[0].Operator != corev1.NodeSelectorOpExists {
		t.Errorf("the plugin does not select exactly the enrolled storage workers: %+v", terms)
	}
}

// The capacity is 100 and is not a setting: the node bootstrap waits for
// allocatable to read exactly that.
func TestTheCapacityIsTheOneTheNodeBootstrapWaitsFor(t *testing.T) {
	r := aWorkloadReconciler(t, aSizedCluster())
	if err := r.reconcileNUMAPlugin(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("reconcile the plugin: %v", err)
	}

	container := mustHaveNUMAPlugin(t, r).Spec.Template.Spec.Containers[0]
	var found bool
	for _, env := range container.Env {
		if env.Name == "NUMA_CAPACITY" {
			found = true
			if env.Value != "100" {
				t.Errorf("NUMA_CAPACITY = %q, want 100", env.Value)
			}
		}
	}
	if !found {
		t.Error("the container sets no NUMA_CAPACITY, so the plugin advertises its own default")
	}
}

// The plugin reads /sys and talks to the kubelet and nothing else, so it runs
// with no privilege.
func TestTheNUMAPluginRunsUnprivileged(t *testing.T) {
	r := aWorkloadReconciler(t, aSizedCluster())
	if err := r.reconcileNUMAPlugin(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("reconcile the plugin: %v", err)
	}

	security := mustHaveNUMAPlugin(t, r).Spec.Template.Spec.Containers[0].SecurityContext
	if security == nil || security.Privileged == nil || *security.Privileged ||
		security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
		security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem ||
		security.Capabilities == nil || len(security.Capabilities.Drop) != 1 ||
		security.Capabilities.Drop[0] != "ALL" {
		t.Errorf("security context = %+v, want unprivileged, read-only, no capabilities", security)
	}
}

// The image is the installation's, and the default is the release the chart pinned.
func TestTheNUMAPluginImageIsConfigurable(t *testing.T) {
	r := aWorkloadReconciler(t, aSizedCluster())
	r.NUMAPluginImage = "registry.example/numa:v9"
	if err := r.reconcileNUMAPlugin(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("reconcile the plugin: %v", err)
	}
	if got := mustHaveNUMAPlugin(t, r).Spec.Template.Spec.Containers[0].Image; got != "registry.example/numa:v9" {
		t.Errorf("image = %q, want the configured one", got)
	}

	fresh := aWorkloadReconciler(t, aSizedCluster())
	if err := fresh.reconcileNUMAPlugin(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("reconcile the plugin: %v", err)
	}
	if got := mustHaveNUMAPlugin(t, fresh).Spec.Template.Spec.Containers[0].Image; got != DefaultNUMAPluginImage {
		t.Errorf("image = %q, want the default %q", got, DefaultNUMAPluginImage)
	}
}

// Nothing in kube-system is owned by a StorageCluster. A reference to a
// namespaced owner in another namespace is one Kubernetes treats as unresolvable
// and garbage-collects the object over.
func TestTheNUMAPluginCarriesNoOwnerReference(t *testing.T) {
	r := aWorkloadReconciler(t, aSizedCluster())
	if err := r.reconcileNUMAPlugin(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("reconcile the plugin: %v", err)
	}

	ds := mustHaveNUMAPlugin(t, r)
	if len(ds.OwnerReferences) != 0 {
		t.Errorf("owner references = %v, want none", ds.OwnerReferences)
	}
	if ds.Labels["storage.simplyblock.io/managed-by"] == "" {
		t.Error("the DaemonSet carries no managed-by label, so nothing says it is the operator's to delete")
	}
}

// Two clusters share the one DaemonSet.
func TestTwoClustersShareOneNUMAPlugin(t *testing.T) {
	r := aWorkloadReconciler(t, aSizedCluster(), anotherCluster())

	for _, cluster := range []*simplyblockv1alpha2.StorageCluster{aSizedCluster(), anotherCluster()} {
		if err := r.reconcileNUMAPlugin(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile the plugin for %s: %v", cluster.Name, err)
		}
	}

	var list appsv1.DaemonSetList
	if err := r.List(context.Background(), &list, client.InNamespace("kube-system")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("%d DaemonSets in kube-system, want the one both clusters share", len(list.Items))
	}
}

// A DaemonSet the chart rendered before the operator took the plugin over is
// reconciled rather than duplicated, and what Helm recorded on it is kept.
func TestAChartRenderedPluginIsTakenOver(t *testing.T) {
	existing := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: "simplyblock-numa-resource-plugin", Namespace: "kube-system",
		Labels:      map[string]string{"app": "simplyblock-numa-resource-plugin"},
		Annotations: map[string]string{"meta.helm.sh/release-name": "simplyblock-operator"},
	}, Spec: appsv1.DaemonSetSpec{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "simplyblock-numa-resource-plugin"}},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "simplyblock-numa-resource-plugin"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "numa-plugin", Image: "old:image"}}},
		},
	}}
	r := aWorkloadReconciler(t, aSizedCluster(), existing)

	if err := r.reconcileNUMAPlugin(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("reconcile the plugin: %v", err)
	}

	ds := mustHaveNUMAPlugin(t, r)
	if ds.Spec.Template.Spec.Containers[0].Image == "old:image" {
		t.Error("the chart's image is still running, so the operator did not take the DaemonSet over")
	}
	if ds.Annotations["meta.helm.sh/release-name"] != "simplyblock-operator" {
		t.Errorf("annotations = %v, want what Helm recorded kept", ds.Annotations)
	}
}

func reconcileRequest(c *simplyblockv1alpha2.StorageCluster) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}
}

// A cluster going away leaves the plugin while another remains.
func TestTheNUMAPluginStaysWhileAnotherClusterRemains(t *testing.T) {
	r := aWorkloadReconciler(t, anotherCluster())
	if err := r.reconcileNUMAPlugin(context.Background(), anotherCluster()); err != nil {
		t.Fatalf("reconcile the plugin: %v", err)
	}

	// a-cluster does not exist, which is what its deletion looks like to a reconcile.
	if _, err := r.Reconcile(context.Background(), reconcileRequest(aSizedCluster())); err != nil {
		t.Fatalf("reconcile the deleted cluster: %v", err)
	}

	mustHaveNUMAPlugin(t, r)
}

// The last cluster takes the plugin with it, and the account it ran as.
func TestTheLastClusterTakesTheNUMAPluginWithIt(t *testing.T) {
	r := aWorkloadReconciler(t)
	if err := r.reconcileNUMAPlugin(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("reconcile the plugin: %v", err)
	}
	// The cluster was never stored, so no cluster exists when the deletion is seen.
	if _, err := r.Reconcile(context.Background(), reconcileRequest(aSizedCluster())); err != nil {
		t.Fatalf("reconcile the deleted cluster: %v", err)
	}

	if _, err := numaDaemonSet(t, r); !apierrors.IsNotFound(err) {
		t.Errorf("the DaemonSet outlived the last cluster (get: %v)", err)
	}
	var account corev1.ServiceAccount
	err := r.Get(context.Background(),
		client.ObjectKey{Namespace: "kube-system", Name: "simplyblock-numa-resource-plugin"}, &account)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the ServiceAccount outlived the last cluster (get: %v)", err)
	}
}

// A cluster that is terminating does not keep the plugin alive.
func TestATerminatingClusterDoesNotKeepTheNUMAPlugin(t *testing.T) {
	terminating := aSizedCluster()
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"storage.simplyblock.io/storagecluster-finalizer"}
	r := aWorkloadReconciler(t, terminating)
	if err := r.reconcileNUMAPlugin(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("reconcile the plugin: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), reconcileRequest(terminating)); err != nil {
		t.Fatalf("reconcile the terminating cluster: %v", err)
	}

	if _, err := numaDaemonSet(t, r); !apierrors.IsNotFound(err) {
		t.Errorf("the DaemonSet outlived a cluster that is going away (get: %v)", err)
	}
}

// A DaemonSet by that name that this operator never marked is somebody else's,
// and the last cluster leaving does not delete it.
func TestAForeignDaemonSetIsNotDeleted(t *testing.T) {
	foreign := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: "simplyblock-numa-resource-plugin", Namespace: "kube-system",
	}}
	r := aWorkloadReconciler(t, foreign)

	if _, err := r.Reconcile(context.Background(), reconcileRequest(aSizedCluster())); err != nil {
		t.Fatalf("reconcile the deleted cluster: %v", err)
	}

	mustHaveNUMAPlugin(t, r)
}
