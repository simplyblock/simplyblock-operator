// The mds-proxy as design-pnfs-mds-vm.md §8.5 and §10 need it: on the host's
// network wherever the metadata server may run, from the metadata server's
// image, and without privilege. The runner is told where it is, the driver
// reconciler applies it while spec.pnfs.mds is set and removes it after, and
// on OpenShift its ServiceAccount may use the hostnetwork-v2 SCC.

package driver

import (
	"context"
	"maps"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func mdsProxy(t *testing.T) *appsv1.DaemonSet {
	t.Helper()
	ds, err := MDSProxyDaemonSet(withMDS(testDriver("simplyblock")), false)
	if err != nil {
		t.Fatalf("MDSProxyDaemonSet: %v", err)
	}
	return ds
}

func TestTheMDSProxyRunsOnTheHostNetworkWhereverTheMDSMay(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	ds := mdsProxy(t)
	_, sts := mdsObjects(t, d)
	pod := ds.Spec.Template.Spec

	if ds.Name != "simplyblock-csi-mds-proxy" || ds.Namespace != d.Namespace {
		t.Errorf("DaemonSet %s/%s, want %s/simplyblock-csi-mds-proxy", ds.Namespace, ds.Name, d.Namespace)
	}
	if !pod.HostNetwork {
		t.Error("the proxy is not on the host's network, where SPDK's listeners accept its connections")
	}
	if !maps.Equal(pod.NodeSelector, sts.Spec.Template.Spec.NodeSelector) {
		t.Errorf("nodeSelector = %v, want the metadata server's %v", pod.NodeSelector, sts.Spec.Template.Spec.NodeSelector)
	}
	if !slices.Equal(pod.Tolerations, sts.Spec.Template.Spec.Tolerations) {
		t.Errorf("tolerations = %v, want the metadata server's %v", pod.Tolerations, sts.Spec.Template.Spec.Tolerations)
	}
	if pod.ServiceAccountName != "simplyblock-csi-mds-proxy-sa" {
		t.Errorf("ServiceAccount %q, want the proxy's own, not the metadata server's %q, which links as the MDS",
			pod.ServiceAccountName, sts.Spec.Template.Spec.ServiceAccountName)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the proxy mounts a ServiceAccount token it never uses")
	}
	// No host path, no Secret: only the ConfigMap of listener ports.
	if len(pod.Volumes) != 1 || pod.Volumes[0].ConfigMap == nil {
		t.Errorf("the proxy mounts %v, want only its ports ConfigMap", pod.Volumes)
	}

	if len(pod.Containers) != 1 {
		t.Fatalf("want one container, got %d", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Image != d.Spec.PNFS.MDS.Image {
		t.Errorf("image = %q, want the metadata server's %q, so both ends speak one protocol", c.Image, d.Spec.PNFS.MDS.Image)
	}
	if !slices.Equal(c.Command, []string{"/usr/bin/mds-proxy"}) {
		t.Errorf("command = %v, want /usr/bin/mds-proxy", c.Command)
	}
	if !slices.Contains(c.Args, "--listen=$(HOST_IP):9811") {
		t.Errorf("args = %v, want it to listen on the node's address only", c.Args)
	}
	if !hasHostIPEnv(c.Env) {
		t.Errorf("env = %v, want HOST_IP from status.hostIP", c.Env)
	}
	// Measured at 5Mi carrying six volumes' local paths, and 1-3Mi idle.
	if mem := c.Resources.Requests[corev1.ResourceMemory]; mem.String() != "16Mi" {
		t.Errorf("memory request %s, want 16Mi", mem.String())
	}
	if mem := c.Resources.Limits[corev1.ResourceMemory]; mem.String() != "256Mi" {
		t.Errorf("memory limit %s, want 256Mi", mem.String())
	}
	if cpu, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Errorf("CPU limit %s: the proxy carries a volume path, which a CPU limit would throttle", cpu.String())
	}
}

func TestTheMDSProxyHasNoPrivilege(t *testing.T) {
	sc := mdsProxy(t).Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("no securityContext")
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Error("privileged")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("privilege escalation is allowed")
	}
	if sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) != 0 {
		t.Errorf("capabilities = %+v, want every one dropped and none added", sc.Capabilities)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser == 0 {
		t.Errorf("runAsNonRoot = %v, runAsUser = %v, want a non-root user", sc.RunAsNonRoot, sc.RunAsUser)
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("the root filesystem is writable")
	}
}

func TestTheMDSRunnerIsToldWhereItsNodesProxyIs(t *testing.T) {
	_, sts := mdsObjects(t, withMDS(testDriver("simplyblock")))
	c := runnerContainer(t, sts)
	if !slices.Contains(c.Args, "--host-proxy=$(HOST_IP):9811") {
		t.Errorf("args = %v, want --host-proxy at the node's address", c.Args)
	}
	if !hasHostIPEnv(c.Env) {
		t.Errorf("env = %v, want HOST_IP from status.hostIP", c.Env)
	}
}

func hasHostIPEnv(env []corev1.EnvVar) bool {
	return slices.ContainsFunc(env, func(e corev1.EnvVar) bool {
		return e.Name == "HOST_IP" && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil &&
			e.ValueFrom.FieldRef.FieldPath == "status.hostIP"
	})
}

func reconcileDriver(t *testing.T, d *simplyblockv1alpha2.SimplyblockDriver, objs ...client.Object) client.Client {
	t.Helper()
	scheme := reconcilerScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, d)...).WithStatusSubresource(d).Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme, Snapshots: fixedSnapshotAPI{served: true}}
	if _, err := r.Reconcile(context.Background(), requestFor(d)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return c
}

func TestADriverWithAMetadataServerRunsTheProxy(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	c := reconcileDriver(t, d)

	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: d.Namespace, Name: "simplyblock-csi-mds-proxy"}, &ds); err != nil {
		t.Fatalf("the proxy DaemonSet: %v", err)
	}
	if owner := metav1ControllerOf(&ds); owner != d.Name {
		t.Errorf("owned by %q, want the driver %q, so deleting the driver removes it", owner, d.Name)
	}

	// The DaemonSet runs before any export has created the metadata server, so
	// its ServiceAccount has to exist without one.
	var sa corev1.ServiceAccount
	key := client.ObjectKey{Namespace: d.Namespace, Name: ds.Spec.Template.Spec.ServiceAccountName}
	if err := c.Get(context.Background(), key, &sa); err != nil {
		t.Fatalf("the proxy's ServiceAccount %s: %v", key.Name, err)
	}
	if owner := metav1ControllerOf(&sa); owner != d.Name {
		t.Errorf("ServiceAccount owned by %q, want the driver %q", owner, d.Name)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("the proxy's ServiceAccount offers a token to mount")
	}
}

func TestADriverWithoutAMetadataServerRemovesTheProxy(t *testing.T) {
	withProxy := withMDS(testDriver("simplyblock"))
	stale, err := MDSProxyDaemonSet(withProxy, false)
	if err != nil {
		t.Fatalf("MDSProxyDaemonSet: %v", err)
	}
	staleSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: withProxy.Namespace, Name: stale.Spec.Template.Spec.ServiceAccountName,
	}}
	for _, obj := range []client.Object{stale, staleSA} {
		if err := setOwnership(withProxy, obj, reconcilerScheme(t)); err != nil {
			t.Fatalf("setOwnership: %v", err)
		}
	}

	d := testDriver("simplyblock")
	c := reconcileDriver(t, d, stale, staleSA)

	var ds appsv1.DaemonSet
	err = c.Get(context.Background(), client.ObjectKeyFromObject(stale), &ds)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the proxy DaemonSet is still there (%v) after spec.pnfs.mds was unset", err)
	}
	var sa corev1.ServiceAccount
	err = c.Get(context.Background(), client.ObjectKeyFromObject(staleSA), &sa)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the proxy's ServiceAccount is still there (%v) after spec.pnfs.mds was unset", err)
	}
}

func TestAProxyDaemonSetTheDriverDoesNotOwnIsLeftAlone(t *testing.T) {
	stale, err := MDSProxyDaemonSet(withMDS(testDriver("simplyblock")), false)
	if err != nil {
		t.Fatalf("MDSProxyDaemonSet: %v", err)
	}
	d := testDriver("simplyblock")
	c := reconcileDriver(t, d, stale)

	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &ds); err != nil {
		t.Errorf("a DaemonSet the driver does not own was removed: %v", err)
	}
}

// openShiftSCCRole is the ClusterRole OpenShift ships for the hostnetwork-v2
// SCC, and what tells the reconciler it runs on OpenShift.
func openShiftSCCRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:openshift:scc:hostnetwork-v2"}}
}

const proxySCCBinding = "simplyblock-csi-mds-proxy-scc-binding"

func proxyContainer(t *testing.T, c client.Client, d *simplyblockv1alpha2.SimplyblockDriver) corev1.Container {
	t.Helper()
	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: d.Namespace, Name: "simplyblock-csi-mds-proxy"}, &ds); err != nil {
		t.Fatalf("the proxy DaemonSet: %v", err)
	}
	return ds.Spec.Template.Spec.Containers[0]
}

// Without the binding, a host-network pod is refused by every SCC a
// ServiceAccount holds by default, and the DaemonSet gets no pods.
func TestOnOpenShiftTheProxyMayUseTheHostNetworkSCC(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	c := reconcileDriver(t, d, openShiftSCCRole())

	var rb rbacv1.RoleBinding
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: d.Namespace, Name: proxySCCBinding}, &rb); err != nil {
		t.Fatalf("the proxy's SCC RoleBinding: %v", err)
	}
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != "system:openshift:scc:hostnetwork-v2" {
		t.Errorf("roleRef = %+v, want the hostnetwork-v2 SCC's ClusterRole", rb.RoleRef)
	}
	want := rbacv1.Subject{Kind: "ServiceAccount", Name: "simplyblock-csi-mds-proxy-sa", Namespace: d.Namespace}
	if len(rb.Subjects) != 1 || rb.Subjects[0] != want {
		t.Errorf("subjects = %+v, want only the proxy's ServiceAccount", rb.Subjects)
	}
	if owner := metav1ControllerOf(&rb); owner != d.Name {
		t.Errorf("RoleBinding owned by %q, want the driver %q", owner, d.Name)
	}

	// hostnetwork-v2 assigns a user from the namespace's range and refuses
	// any other.
	sc := proxyContainer(t, c, d).SecurityContext
	if sc.RunAsUser != nil {
		t.Errorf("runAsUser = %d, want it left to the SCC", *sc.RunAsUser)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("runAsNonRoot is not set")
	}
}

func TestOffOpenShiftTheProxyGetsNoSCCBinding(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	c := reconcileDriver(t, d)

	var rb rbacv1.RoleBinding
	err := c.Get(context.Background(), client.ObjectKey{Namespace: d.Namespace, Name: proxySCCBinding}, &rb)
	if !apierrors.IsNotFound(err) {
		t.Errorf("an SCC RoleBinding on a cluster without SCCs (%v)", err)
	}
	if sc := proxyContainer(t, c, d).SecurityContext; sc.RunAsUser == nil || *sc.RunAsUser != 65534 {
		t.Errorf("runAsUser = %v, want 65534, which the image's root user needs set", sc.RunAsUser)
	}
}

func TestADriverWithoutAMetadataServerRemovesTheSCCBinding(t *testing.T) {
	withProxy := withMDS(testDriver("simplyblock"))
	stale := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: withProxy.Namespace, Name: proxySCCBinding}}
	if err := setOwnership(withProxy, stale, reconcilerScheme(t)); err != nil {
		t.Fatalf("setOwnership: %v", err)
	}
	c := reconcileDriver(t, testDriver("simplyblock"), stale, openShiftSCCRole())

	var rb rbacv1.RoleBinding
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &rb); !apierrors.IsNotFound(err) {
		t.Errorf("the SCC RoleBinding is still there (%v) after spec.pnfs.mds was unset", err)
	}
}

// storageNode is a StorageNode whose LVS listens on lvol.
func storageNode(name string, lvol int32) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
		Status:     simplyblockv1alpha2.StorageNodeStatus{Ports: &simplyblockv1alpha2.StorageNodePorts{Lvol: ptr.To(lvol)}},
	}
}

// Without an attachment to the data network the guest reaches every storage node
// through the proxy, which opens connections to other nodes only on the storage
// cluster's listener ports. The operator writes them, from every StorageNode's
// LVS port, into a ConfigMap the proxy reads, so no data network, subnet, or IP
// range has to be configured: on OVN-Kubernetes a pod's traffic to the data
// network never arrives, and a dataNetwork set by hand was lost twice in a day
// on dev-ocp.
func TestTheMDSProxyIsToldTheStorageListenerPorts(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	c := reconcileDriver(t, d, storageNode("n1", 4431), storageNode("n2", 4429), storageNode("n3", 4431))

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: d.Namespace, Name: "simplyblock-csi-mds-proxy-ports"}, &cm); err != nil {
		t.Fatalf("the ports ConfigMap: %v", err)
	}
	if got := cm.Data["ports"]; got != "4429\n4431\n" {
		t.Errorf("ports = %q, want each LVS port once, sorted", got)
	}
	if owner := metav1ControllerOf(&cm); owner != d.Name {
		t.Errorf("ConfigMap owned by %q, want the driver", owner)
	}

	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: d.Namespace, Name: "simplyblock-csi-mds-proxy"}, &ds); err != nil {
		t.Fatalf("the proxy DaemonSet: %v", err)
	}
	pod := ds.Spec.Template.Spec
	if !slices.Contains(pod.Containers[0].Args, "--ports-file=/etc/mds-proxy/ports") {
		t.Errorf("args = %v, want the ports file", pod.Containers[0].Args)
	}
	mounted := slices.ContainsFunc(pod.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.MountPath == "/etc/mds-proxy" && m.ReadOnly
	})
	fromConfigMap := slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool {
		return v.ConfigMap != nil && v.ConfigMap.Name == "simplyblock-csi-mds-proxy-ports"
	})
	if !mounted || !fromConfigMap {
		t.Errorf("volumes = %+v, mounts = %+v, want the ConfigMap read-only at /etc/mds-proxy", pod.Volumes, pod.Containers[0].VolumeMounts)
	}
}

// The ports ConfigMap goes with the proxy once spec.pnfs.mds is unset.
func TestADriverWithoutAMetadataServerRemovesThePortsConfigMap(t *testing.T) {
	withProxy := withMDS(testDriver("simplyblock"))
	stale := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: withProxy.Namespace, Name: "simplyblock-csi-mds-proxy-ports"}}
	if err := setOwnership(withProxy, stale, reconcilerScheme(t)); err != nil {
		t.Fatalf("setOwnership: %v", err)
	}
	c := reconcileDriver(t, testDriver("simplyblock"), stale)

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &cm); !apierrors.IsNotFound(err) {
		t.Errorf("the ports ConfigMap is still there (%v)", err)
	}
}

// metav1ControllerOf names the controlling owner of obj, or returns the empty
// string when it has none.
func metav1ControllerOf(obj client.Object) string {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return ref.Name
		}
	}
	return ""
}
