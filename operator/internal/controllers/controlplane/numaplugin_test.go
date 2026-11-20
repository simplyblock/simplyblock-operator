// The NUMA device plugin the control plane's owner runs on the storage workers.
//
// The plugin advertises numa-align/numa-<socket> on each storage worker, and the
// storage pods request it so that a pod lands next to the memory and devices of
// one socket. It is one DaemonSet for the Kubernetes cluster, in the operator's
// namespace and owned by the ControlPlane singleton, whichever source that names:
// a remote control plane still has storage workers beside it.

package controlplane

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// reconciledManaged reconciles a remote control plane, which installs nothing
// else, so what it writes is the plugin and nothing besides.
func reconciledManaged(t *testing.T, image string) (client.Client, *simplyblockv1alpha2.ControlPlane) {
	t.Helper()
	cp := managedControlPlane("https://sb-control.example.com:5000")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-token", Namespace: testNamespace},
		Data:       map[string][]byte{"token": []byte("a-bearer-token")},
	}
	c := newClient(t, cp, secret)
	r := &ControlPlaneReconciler{
		Client: c, Scheme: testScheme(t), Prober: &stubProber{ready: true}, NUMAPluginImage: image,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return c, cp
}

func numaPlugin(t *testing.T, c client.Client) *appsv1.DaemonSet {
	t.Helper()
	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: "simplyblock-numa-resource-plugin"}, &ds); err != nil {
		t.Fatalf("the NUMA plugin DaemonSet does not exist in the operator's namespace: %v", err)
	}
	return &ds
}

// The plugin is owned by the ControlPlane, so removing it removes the plugin, and
// it runs on every worker any StorageCluster enrolled.
func TestTheControlPlaneOwnsTheNUMAPlugin(t *testing.T) {
	c, cp := reconciledManaged(t, "")

	ds := numaPlugin(t, c)
	if len(ds.OwnerReferences) != 1 || ds.OwnerReferences[0].Kind != "ControlPlane" ||
		ds.OwnerReferences[0].Name != cp.Name {
		t.Errorf("owner references = %+v, want the ControlPlane", ds.OwnerReferences)
	}
	terms := ds.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if req := terms[0].MatchExpressions[0]; req.Key != "io.simplyblock.storagenodeset" ||
		req.Operator != corev1.NodeSelectorOpExists {
		t.Errorf("node affinity = %+v, want every enrolled worker", req)
	}
	// The DaemonSet's selector matches the pods by this label.
	if ds.Spec.Template.Labels["app"] != "simplyblock-numa-resource-plugin" {
		t.Errorf("pod labels = %v, want the app label the DaemonSet selects", ds.Spec.Template.Labels)
	}

	var account corev1.ServiceAccount
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: testNamespace, Name: ds.Spec.Template.Spec.ServiceAccountName,
	}, &account); err != nil {
		t.Fatalf("the account the plugin runs as does not exist: %v", err)
	}
}

// The image is the installation's: a mirror is how a disconnected installation
// runs the plugin.
func TestTheNUMAPluginImageIsConfigurable(t *testing.T) {
	c, _ := reconciledManaged(t, "registry.example/numa:v9")
	if got := numaPlugin(t, c).Spec.Template.Spec.Containers[0].Image; got != "registry.example/numa:v9" {
		t.Errorf("image = %q, want the configured one", got)
	}
}

// The plugin runs as root whatever the image's USER is. It creates its socket in
// kubelet's root-owned /var/lib/kubelet/device-plugins with every capability
// dropped, so only uid 0 can register it, and a certified image runs non-root.
func TestTheNUMAPluginRunsAsRoot(t *testing.T) {
	c, _ := reconciledManaged(t, "")
	sc := numaPlugin(t, c).Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Errorf("securityContext.runAsUser = %v, want 0", sc)
	}
	if sc != nil && (sc.Privileged == nil || *sc.Privileged || len(sc.Capabilities.Drop) == 0) {
		t.Error("running as root must not widen the rest: privileged stays false and every capability stays dropped")
	}
}

// numaPluginSELinuxTypeOn is the SELinux type of the plugin's container as the
// reconciler of an OpenShift cluster, or of any other, builds it.
func numaPluginSELinuxTypeOn(t *testing.T, openShift bool) string {
	t.Helper()
	r := &ControlPlaneReconciler{OpenShift: openShift}
	objs, err := r.numaPluginObjects(localControlPlane())
	if err != nil {
		t.Fatal(err)
	}
	sc := objs[1].(*appsv1.DaemonSet).Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil || sc.SELinuxOptions == nil {
		t.Fatalf("securityContext = %+v, want an SELinux type stated", sc)
	}
	if sc.Privileged == nil || *sc.Privileged || sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 {
		t.Error("the SELinux type must not widen the rest: privileged stays false and every capability stays dropped")
	}
	return sc.SELinuxOptions.Type
}

// Regression: 2026-11-13, RKE2 1.36 on Rocky 9.5 with SELinux enforcing. RKE2 runs
// its kubelet as container_runtime_t, and container_device_plugin_t may not
// connect to its socket (avc: denied { connectto } ... kubelet.sock), so the
// plugin never registered and the node advertised no numa-align. With spc_t on the
// same node it registered.
func TestTheNUMAPluginIsNotConfinedToOpenShiftsKubeletOffOpenShift(t *testing.T) {
	if got := numaPluginSELinuxTypeOn(t, false); got != "spc_t" {
		t.Errorf("SELinux type off OpenShift = %q, want spc_t", got)
	}
}

// OpenShift's policy allows container_device_plugin_t to register with its
// kubelet, and its security context constraints do not hand out spc_t to an
// unprivileged pod.
func TestTheNUMAPluginKeepsTheDevicePluginTypeOnOpenShift(t *testing.T) {
	if got := numaPluginSELinuxTypeOn(t, true); got != "container_device_plugin_t" {
		t.Errorf("SELinux type on OpenShift = %q, want container_device_plugin_t", got)
	}
}

// Without SB_NUMA_PLUGIN_IMAGE the plugin runs a pinned release, never latest:
// the DaemonSet pulls on every restart, so a moving tag reaches every cluster.
func TestTheNUMAPluginDefaultsToAPinnedRelease(t *testing.T) {
	c, _ := reconciledManaged(t, "")
	if got := numaPlugin(t, c).Spec.Template.Spec.Containers[0].Image; got != "quay.io/simplyblock-io/numa-resource-plugin:v1.0.0" {
		t.Errorf("default image = %q, want quay.io/simplyblock-io/numa-resource-plugin:v1.0.0", got)
	}
}
