// The probe pods: the guest probe attached to the declared networks the way a
// storage node's SPDK pod will be, and the host probe on the host network that
// pings it. Both are one-shot pods pinned to the worker, owned by the StorageNode,
// and unprivileged beyond the capability a raw socket needs.
//
// Specified by operator/docs/designs/design-storage-network.md §8.1.

package netprobe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	"github.com/simplyblock/simplyblock-operator/internal/multus"
)

const (
	// KindLabel says which probe a pod is, and NodeLabel which StorageNode it
	// probes for. They are what the gate lists by.
	KindLabel = "storage.simplyblock.io/network-probe"
	NodeLabel = "storage.simplyblock.io/network-probe-node"

	// KindGuest and KindHost are the two values of KindLabel.
	KindGuest = "guest"
	KindHost  = "host"

	// CheckContainer is the container that runs the checks and writes the
	// result, and HoldContainer the one that keeps the guest's network after the
	// checks finish.
	CheckContainer = "netprobe"
	HoldContainer  = "hold"

	// maxName is the longest a pod name may be and still be a label.
	maxName = 63
)

// interfaceName is what an interface name inside a pod may look like: the
// characters the kernel allows without a space, a slash, or a quote.
var interfaceName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,15}$`)

// InterfaceCheck is what the guest probe checks on one interface: that no other
// host holds the address, and that each peer answers.
type InterfaceCheck struct {
	Interface string
	// Address is the declared address without its prefix length.
	Address string

	// Candidates are addresses the probe only asks about, with the same
	// duplicate-address check as Address, so that the node learns which of them
	// are taken without attaching any.
	Candidates []string
	Peers      []string
}

// GuestOptions describes a guest probe.
type GuestOptions struct {
	Namespace string
	// Node is the StorageNode the probe is for, and Worker the Kubernetes node it
	// runs on.
	Node   string
	Worker string
	Image  string
	Owner  metav1.OwnerReference

	// Networks is the networks annotation the SPDK pod will carry.
	Networks string

	Checks []InterfaceCheck
}

// HostOptions describes a host probe.
type HostOptions struct {
	Namespace string
	Node      string
	Worker    string
	Image     string
	Owner     metav1.OwnerReference

	// Targets are the addresses to ping from the host network namespace.
	Targets []string
}

// Name is a probe pod's name, short enough to be a label. A long node name is
// cut and given a hash of the whole, so two long names never share a probe.
func Name(node, kind string) string {
	name := fmt.Sprintf("netprobe-%s-%s", kind, node)
	if len(name) <= maxName {
		return name
	}
	sum := sha256.Sum256([]byte(node))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	return strings.TrimRight(name[:maxName-len(suffix)], "-.") + suffix
}

// GuestPod builds the guest probe. It refuses an interface name or an address
// that is not what it claims to be, because they reach a shell.
func GuestPod(opts GuestOptions) (*corev1.Pod, error) {
	var ifaces, arping, peers []string
	for _, check := range opts.Checks {
		if !interfaceName.MatchString(check.Interface) {
			return nil, fmt.Errorf("%q is not an interface name", check.Interface)
		}
		if err := requireIPv4(check.Address); err != nil {
			return nil, err
		}
		ifaces = append(ifaces, check.Interface)
		arping = append(arping, check.Interface+"="+check.Address)
		for _, candidate := range check.Candidates {
			if err := requireIPv4(candidate); err != nil {
				return nil, err
			}
			arping = append(arping, check.Interface+"="+candidate)
		}
		for _, peer := range check.Peers {
			if err := requireIPv4(peer); err != nil {
				return nil, err
			}
			peers = append(peers, check.Interface+"="+peer)
		}
	}

	pod := basePod(opts.Namespace, opts.Node, opts.Worker, opts.Image, opts.Owner, KindGuest, guestScript, []corev1.EnvVar{
		{Name: "IFACES", Value: strings.Join(ifaces, ",")},
		{Name: "ARPING", Value: strings.Join(arping, ",")},
		{Name: "PEERS", Value: strings.Join(peers, ",")},
	})
	pod.Annotations = map[string]string{multus.NetworksAnnotation: opts.Networks}

	// The host probe pings this pod's address, and a pod whose only container has
	// exited gives its network namespace back. The holder keeps it until the pod
	// is deleted, and the pod's deadline ends it if nothing deletes the pod.
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:            HoldContainer,
		Image:           opts.Image,
		Command:         []string{"/bin/sh", "-c", "exec sleep " + strconv.Itoa(deadlineSeconds)},
		SecurityContext: pod.Spec.Containers[0].SecurityContext.DeepCopy(),
	})
	return pod, nil
}

// HostPod builds the host probe.
func HostPod(opts HostOptions) (*corev1.Pod, error) {
	for _, target := range opts.Targets {
		if err := requireIPv4(target); err != nil {
			return nil, err
		}
	}
	pod := basePod(opts.Namespace, opts.Node, opts.Worker, opts.Image, opts.Owner, KindHost, hostScript, []corev1.EnvVar{
		{Name: "TARGETS", Value: strings.Join(opts.Targets, ",")},
	})
	pod.Spec.HostNetwork = true
	return pod, nil
}

// deadlineSeconds bounds a probe pod's life, so that one the operator lost track
// of does not hold an address for longer.
const deadlineSeconds = 600

// terminationGraceSeconds is how long a deleted probe may take to stop.
const terminationGraceSeconds = 1

// root is the user the probes run as. A raw socket needs the NET_RAW capability
// to be effective, and an image whose default user is not root would not have it.
var root = int64(0)

// basePod is what both probes share.
func basePod(
	namespace, node, worker, image string, owner metav1.OwnerReference, kind, script string, env []corev1.EnvVar,
) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: Name(node, kind), Namespace: namespace,
			Labels:          map[string]string{KindLabel: kind, NodeLabel: labelValue(node)},
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: ptr.To(int64(deadlineSeconds)),
			// A probe holds nothing worth a grace period, and a slow exit keeps the
			// node's next probe from starting under the same name.
			TerminationGracePeriodSeconds: ptr.To(int64(terminationGraceSeconds)),
			NodeSelector:                  map[string]string{"kubernetes.io/hostname": worker},
			Tolerations:                   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:                   CheckContainer,
				Image:                  image,
				Command:                []string{"/bin/sh", "-c", script},
				Env:                    env,
				TerminationMessagePath: "/dev/termination-log",
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                &root,
					AllowPrivilegeEscalation: new(bool),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"NET_RAW"},
					},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			}},
		},
	}
}

// requireIPv4 accepts a bare IPv4 address and nothing else.
func requireIPv4(value string) error {
	addr, err := netip.ParseAddr(value)
	if err != nil || !addr.Is4() {
		return fmt.Errorf("%q is not an IPv4 address", value)
	}
	return nil
}

// labelValue is a node name as a label value, cut to the length a label allows.
func labelValue(name string) string {
	if len(name) > maxName {
		return strings.TrimRight(name[:maxName], "-._")
	}
	return name
}
