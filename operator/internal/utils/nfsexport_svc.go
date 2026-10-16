// The stable client address for a metadata server's pNFS exports: one
// ClusterIP Service per metadata server and the single-endpoint EndpointSlice
// behind it.
//
// A client mounts the Service, never the MDS host directly, and never
// resolves it more than once: an NFS mount is a kernel sunrpc socket
// connected to whatever address it resolved to at mount time, so a plain DNS
// name would freeze the client onto the host that happened to be bound then.
// A ClusterIP does not have this problem, because nothing resolves it more
// than once either -- kube-proxy's DNAT rule is what actually points traffic
// at the current host, and the operator rewrites that rule (by rewriting the
// EndpointSlice) rather than the address the client holds. See
// design-pnfs-rwx.md §13.3.
package utils

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// NFSExportServicePort is the port nfsd binds in the host network namespace,
// and so the port every export's Service targets. It never varies by export:
// nfsd is one server per host serving every export bound to it, distinguished
// by path and fsid, not by port.
const NFSExportServicePort = 2049

// buildEndpointSlice publishes the metadata server pod as the Service's
// one endpoint. Repointing an export at a different host is rebuilding this
// with the same name and a different address; the Service's ClusterIP does
// not change, so a client's mount address does not either.
func buildEndpointSlice(namespace, serviceName, mdsNodeIP string) *discoveryv1.EndpointSlice {
	protocol := corev1.ProtocolTCP
	port := int32(NFSExportServicePort)
	portName := "nfs"
	ready := true

	// No address is an EndpointSlice with no endpoint: the metadata server is
	// going away and kube-proxy refuses new connections rather than sending them
	// to a pod that will not answer (design-pnfs-mds-vm.md §8.1).
	var endpoints []discoveryv1.Endpoint
	if mdsNodeIP != "" {
		endpoints = []discoveryv1.Endpoint{{
			Addresses:  []string{mdsNodeIP},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}}
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      MDSEndpointSliceName(serviceName),
			Namespace: namespace,
			Labels: map[string]string{
				"kubernetes.io/service-name": serviceName,
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
		Ports: []discoveryv1.EndpointPort{
			{
				Name:     &portName,
				Protocol: &protocol,
				Port:     &port,
			},
		},
	}
}

// MDSServiceName is the Service every export of a metadata server is mounted
// at, named after the metadata server's StatefulSet, whose one pod is podName.
// The StatefulSet's name leaves room for the suffix within the 63 characters a
// Service name takes.
func MDSServiceName(podName string) string {
	return strings.TrimSuffix(podName, "-0") + "-nfs"
}

// MDSEndpointSliceName names the one EndpointSlice backing a metadata server's
// Service.
func MDSEndpointSliceName(serviceName string) string {
	return serviceName + "-endpoints"
}

// BuildMDSService is the Service a client mounts every export of one metadata
// server at. One address for one server: the Linux client keeps one NFS
// client per server and trunks a mount at any other address of the same
// server onto it, so an address per export made every mount on a node depend
// on whichever export it mounted first.
func BuildMDSService(namespace, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:       "nfs",
				Port:       NFSExportServicePort,
				TargetPort: intstr.FromInt32(NFSExportServicePort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// BuildMDSEndpointSlice publishes the metadata server pod's address as its
// Service's one endpoint, or none while the metadata server is going away.
func BuildMDSEndpointSlice(namespace, serviceName, address string) *discoveryv1.EndpointSlice {
	return buildEndpointSlice(namespace, serviceName, address)
}
