// The mds-proxy: a DaemonSet on the host's network that opens the metadata
// server guest's connections to the storage node on its own node, which that
// storage node refuses from a pod (design-pnfs-mds-vm.md §8.5).
//
// It lives beside the metadata server's StatefulSet because it shares the
// metadata server's image, node selector, and tolerations. It
// is applied by the driver reconciler while spec.pnfs.mds is set, rather than
// with the StatefulSet on the first export, so it already runs, and has pulled
// the image, wherever the metadata server starts.
//
// On OpenShift a host-network pod needs an SCC that allows it, which no
// ServiceAccount holds by default. The proxy's is bound to hostnetwork-v2, the
// narrowest one OpenShift ships for it, and leaves its user to that SCC.

package driver

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// mdsProxyPort is where the proxy listens on its node's address, and where
	// the runner reaches it.
	mdsProxyPort = 9811

	// mdsProxyUser is the proxy's user: nobody, which owns nothing in the
	// image and needs nothing, since the port is unprivileged.
	mdsProxyUser = 65534

	// hostIPEnv carries the node's address (status.hostIP) into both ends.
	hostIPEnv = "HOST_IP"

	// mdsProxyPortsDir and mdsProxyPortsKey are where the proxy reads the
	// storage cluster's listener ports, the only ports it dials on another
	// node: the ConfigMap's one key, mounted as a file.
	mdsProxyPortsDir = "/etc/mds-proxy"
	mdsProxyPortsKey = "ports"

	// hostNetworkSCCRole is the ClusterRole OpenShift ships to grant use of
	// the hostnetwork-v2 SCC, from 4.11 on. Its presence is what says the
	// cluster is OpenShift.
	hostNetworkSCCRole = "system:openshift:scc:hostnetwork-v2"
)

// mdsProxyAddress is the proxy's host:port as both containers spell it, with
// the node's address substituted by kubelet from hostIPEnv.
var mdsProxyAddress = "$(" + hostIPEnv + "):" + strconv.Itoa(mdsProxyPort)

// mdsProxyName is the proxy's DaemonSet.
func mdsProxyName(d *simplyblockv1alpha2.SimplyblockDriver) string {
	return names(d).prefix + "mds-proxy"
}

// mdsProxyPortsName is the ConfigMap of the storage cluster's listener ports.
func mdsProxyPortsName(d *simplyblockv1alpha2.SimplyblockDriver) string {
	return names(d).prefix + "mds-proxy-ports"
}

// MDSProxyPortsConfigMap lists the storage cluster's listener ports for the
// proxy, one per line, each once and sorted. The caller sets ownership.
//
// Without an attachment to the data network the guest reaches every storage
// node through the proxy, because the host routes to the data network wherever
// the pod network does not (OVN-Kubernetes sends it to the gateway). The proxy
// dials another node only on these ports, so it is no relay into the host's
// network, and the listeners there authorize every connect by host NQN.
func MDSProxyPortsConfigMap(d *simplyblockv1alpha2.SimplyblockDriver, ports []int32) *corev1.ConfigMap {
	slices.Sort(ports)
	ports = slices.Compact(ports)
	var b strings.Builder
	for _, p := range ports {
		b.WriteString(strconv.Itoa(int(p)))
		b.WriteByte('\n')
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: mdsProxyPortsName(d), Namespace: d.Namespace},
		Data:       map[string]string{mdsProxyPortsKey: b.String()},
	}
}

// storageListenerPorts is every StorageNode's LVS port: the ports the guest's
// volumes are served on.
func (r *SimplyblockDriverReconciler) storageListenerPorts(ctx context.Context) ([]int32, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("listing StorageNodes for the mds-proxy's ports: %w", err)
	}
	var ports []int32
	for _, n := range nodes.Items {
		if n.Status.Ports != nil && n.Status.Ports.Lvol != nil && *n.Status.Ports.Lvol > 0 {
			ports = append(ports, *n.Status.Ports.Lvol)
		}
	}
	return ports, nil
}

// mdsNodeSelector is where the metadata server may run: the KVM-capable nodes
// matching spec.pnfs.mds.nodeSelector.
func mdsNodeSelector(spec *simplyblockv1alpha2.DriverPNFSMDS) map[string]string {
	selector := maps.Clone(spec.NodeSelector)
	if selector == nil {
		selector = map[string]string{}
	}
	selector[KVMCapableLabel] = KVMCapableValue
	return selector
}

// hostIPEnvVar hands a container its node's address.
func hostIPEnvVar() corev1.EnvVar {
	return corev1.EnvVar{
		Name: hostIPEnv,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"},
		},
	}
}

// MDSProxyServiceAccount renders the proxy's ServiceAccount, which carries no
// permissions and offers no token. The caller sets ownership.
func MDSProxyServiceAccount(d *simplyblockv1alpha2.SimplyblockDriver) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: names(d).mdsProxyServiceAccount, Namespace: d.Namespace},
		AutomountServiceAccountToken: ptr.To(false),
	}
}

// mdsProxySCCBinding lets the proxy's ServiceAccount use the hostnetwork-v2
// SCC. The caller sets ownership.
//
// rbac-justified: the proxy runs on the host's network (design-pnfs-mds-vm.md
// §8.5), which OpenShift admits only under an SCC allowing it. hostnetwork-v2
// is the narrowest one shipped, the binding is namespaced to one account, and
// creating it uses the manager's existing bind on ClusterRoles.
func mdsProxySCCBinding(d *simplyblockv1alpha2.SimplyblockDriver) *rbacv1.RoleBinding {
	n := names(d)
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: n.roleBinding("mds-proxy-scc"), Namespace: d.Namespace},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      n.mdsProxyServiceAccount,
			Namespace: d.Namespace,
		}},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: hostNetworkSCCRole},
	}
}

// MDSProxyDaemonSet renders the proxy for a driver whose spec.pnfs.mds is set.
// On OpenShift the SCC picks the user. The caller sets ownership.
func MDSProxyDaemonSet(d *simplyblockv1alpha2.SimplyblockDriver, openShift bool) (*appsv1.DaemonSet, error) {
	spec := d.Spec.PNFS.MDS
	if spec == nil {
		return nil, fmt.Errorf("spec.pnfs.mds is unset, so there is no metadata server to proxy for")
	}
	image, err := mdsImage(d)
	if err != nil {
		return nil, err
	}
	name := mdsProxyName(d)
	labels := map[string]string{"app": name}
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: d.Namespace},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{utils.AnnotationLogCollector: "true"},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:           names(d).mdsProxyServiceAccount,
					AutomountServiceAccountToken: ptr.To(false),
					NodeSelector:                 mdsNodeSelector(spec),
					Tolerations:                  spec.Tolerations,
					// rbac-justified: the proxy opens the guest's connections
					// to its node's storage node from the host, because SPDK's
					// listeners are bound to the node's data interface and
					// refuse a pod's connection to its own node. It holds no
					// privilege, capability, or host path (design-pnfs-mds-vm.md
					// §10).
					HostNetwork: true,
					DNSPolicy:   corev1.DNSClusterFirstWithHostNet,
					Containers:  []corev1.Container{mdsProxyContainer(d, image, openShift)},
					Volumes:     []corev1.Volume{configMapVolume("ports", mdsProxyPortsName(d), true)},
				},
			},
		},
	}, nil
}

func mdsProxyContainer(d *simplyblockv1alpha2.SimplyblockDriver, image string, openShift bool) corev1.Container {
	// The image's user is root, so elsewhere the proxy names one. hostnetwork-v2
	// assigns one from the namespace's range and refuses any other.
	user := ptr.To(int64(mdsProxyUser))
	if openShift {
		user = nil
	}
	return corev1.Container{
		Name:            "mds-proxy",
		Image:           image,
		ImagePullPolicy: pullPolicy(d),
		Command:         []string{"/usr/bin/mds-proxy"},
		Args: []string{
			"--listen=" + mdsProxyAddress,
			"--ports-file=" + mdsProxyPortsDir + "/" + mdsProxyPortsKey,
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "ports", MountPath: mdsProxyPortsDir, ReadOnly: true}},
		Env:          []corev1.EnvVar{hostIPEnvVar()},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                user,
			ReadOnlyRootFilesystem:   ptr.To(true),
		},
		// No CPU limit: the proxy carries one path of the guest's volumes, and
		// throttling it would throttle their I/O. A relayed connection costs about
		// 70KiB, and NVMe/TCP opens one per queue, so 100 volumes at five queues
		// each need about 35Mi: the memory limit only catches a leak.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		Ports: []corev1.ContainerPort{{Name: "proxy", ContainerPort: mdsProxyPort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: named("proxy")}},
			PeriodSeconds: 10,
		},
	}
}

// ensureMDSProxy applies the proxy and its ServiceAccount while spec.pnfs.mds
// is set and deletes the ones this driver owns once it is not. A metadata
// server image that cannot be resolved is reported and leaves the proxy as it
// is: the metadata server cannot start either, and that is reported where it
// fails.
func (r *SimplyblockDriverReconciler) ensureMDSProxy(ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver) error {
	binding := mdsProxySCCBinding(d)
	if d.Spec.PNFS.MDS == nil {
		if err := r.deleteOwned(ctx, d, &appsv1.DaemonSet{}, mdsProxyName(d)); err != nil {
			return err
		}
		if err := r.deleteOwned(ctx, d, &rbacv1.RoleBinding{}, binding.Name); err != nil {
			return err
		}
		if err := r.deleteOwned(ctx, d, &corev1.ConfigMap{}, mdsProxyPortsName(d)); err != nil {
			return err
		}
		return r.deleteOwned(ctx, d, &corev1.ServiceAccount{}, names(d).mdsProxyServiceAccount)
	}

	openShift, err := r.hostNetworkSCCServed(ctx)
	if err != nil {
		return err
	}
	ds, err := MDSProxyDaemonSet(d, openShift)
	if err != nil {
		r.event(d, corev1.EventTypeWarning, reasonNoImage, fmt.Sprintf("cannot run the mds-proxy: %v", err))
		return nil
	}
	// The ServiceAccount and its SCC first: a DaemonSet whose pods would be
	// refused backs off before it tries again.
	ports, err := r.storageListenerPorts(ctx)
	if err != nil {
		return err
	}
	objs := []client.Object{MDSProxyServiceAccount(d), MDSProxyPortsConfigMap(d, ports)}
	if openShift {
		objs = append(objs, binding)
	}
	for _, obj := range append(objs, ds) {
		if err := r.applyOwned(ctx, d, obj); err != nil {
			return err
		}
	}
	return nil
}

// hostNetworkSCCServed reports whether the cluster ships the hostnetwork-v2
// SCC's ClusterRole, which is what makes it OpenShift for the proxy.
func (r *SimplyblockDriverReconciler) hostNetworkSCCServed(ctx context.Context) (bool, error) {
	err := r.Get(ctx, client.ObjectKey{Name: hostNetworkSCCRole}, &rbacv1.ClusterRole{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("reading the ClusterRole %s: %w", hostNetworkSCCRole, err)
	}
}

// applyOwned applies obj owned by the driver.
func (r *SimplyblockDriverReconciler) applyOwned(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver, obj client.Object,
) error {
	kind := fmt.Sprintf("%T %s", obj, obj.GetName())
	if err := setOwnership(d, obj, r.Scheme); err != nil {
		return fmt.Errorf("set ownership on %s: %w", kind, err)
	}
	cfg, err := r.applyConfiguration(obj)
	if err != nil {
		return fmt.Errorf("encode %s for apply: %w", kind, err)
	}
	if err := r.Apply(ctx, cfg, fieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s: %w", kind, err)
	}
	return nil
}

// deleteOwned deletes the object named name into obj when the driver controls
// it, and leaves one it does not control alone.
func (r *SimplyblockDriverReconciler) deleteOwned(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver, obj client.Object, name string,
) error {
	kind := fmt.Sprintf("%T %s", obj, name)
	if err := r.Get(ctx, client.ObjectKey{Namespace: d.Namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", kind, err)
	}
	if !metav1.IsControlledBy(obj, d) {
		return nil
	}
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting %s: %w", kind, err)
	}
	return nil
}
