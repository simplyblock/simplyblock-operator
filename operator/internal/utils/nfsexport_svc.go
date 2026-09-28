// The stable client address for a pNFS export: a ClusterIP Service and the
// single-endpoint EndpointSlice behind it.
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

// NFSExportServiceName is the ClusterIP Service a client mounts for the named
// export, in place of the MDS host's own address.
func NFSExportServiceName(exportName string) string {
	return exportName + "-nfs"
}

// NFSExportEndpointSliceName names the one EndpointSlice backing an export's
// Service. One export, one slice, one endpoint: the bound MDS host.
func NFSExportEndpointSliceName(exportName string) string {
	return exportName + "-nfs-endpoints"
}

// BuildNFSExportService is the Service a client mounts.
//
// A normal ClusterIP, not the headless, per-node-hostname pattern
// BuildStorageNodeSetService uses: that pattern hands out one DNS name per
// node so a caller can address a specific one by name, which is the opposite
// of what an export needs -- one address that keeps working when the node
// behind it changes.
func BuildNFSExportService(namespace, exportName string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NFSExportServiceName(exportName),
			Namespace: namespace,
			Labels: map[string]string{
				"storage.simplyblock.io/nfsexport": exportName,
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       "nfs",
					Port:       NFSExportServicePort,
					TargetPort: intstr.FromInt32(NFSExportServicePort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// BuildNFSExportEndpointSlice publishes the bound MDS host as the Service's
// one endpoint. Repointing an export at a different host is rebuilding this
// with the same name and a different address; the Service's ClusterIP does
// not change, so a client's mount address does not either.
func BuildNFSExportEndpointSlice(namespace, exportName, mdsNodeIP string) *discoveryv1.EndpointSlice {
	protocol := corev1.ProtocolTCP
	port := int32(NFSExportServicePort)
	portName := "nfs"
	ready := true

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NFSExportEndpointSliceName(exportName),
			Namespace: namespace,
			Labels: map[string]string{
				"kubernetes.io/service-name": NFSExportServiceName(exportName),
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{mdsNodeIP},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{
				Name:     &portName,
				Protocol: &protocol,
				Port:     &port,
			},
		},
	}
}
