// The two ConfigMaps both plugins mount.
//
// Neither carries the control plane's address or credentials, and that is not an
// omission. The driver reads those from simplyblock-csi-secret-v2, which the
// StorageCluster reconciler upserts with one entry per cluster it creates or
// adopts (internal/controller/simplyblockstoragecluster_controller.go,
// upsertCSICredentialsSecret). That Secret is therefore not in this
// deployment's object set: two controllers writing one object alternate its
// contents, which is the failure design §3.4 keeps a cluster to one driver to
// avoid, and it would be no better between two kinds than between two drivers.
//
// So this deployment mounts the Secret and does not own it, and what it owns
// here is the pair of ConfigMaps that carry no cluster state at all.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.1.

package driver

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// legacyConfigJSON is what the chart renders into simplyblock-csi-cm at default
// values, and what every measured deployment is running. The fields are null
// because the driver takes the endpoint and the cluster's identity from the
// credentials Secret instead; the file is still mounted, so it still has to
// exist and still has to parse.
const legacyConfigJSON = `{"simplybk":{"ip":null,"uuid":null}}`

// nodeServerConfigJSON configures the xPU offload targets, of which a deployment
// that is not running an xPU has none. The file is mounted optional, so an empty
// one is the same as no file, and it is written for the deployments that later
// gain one.
const nodeServerConfigJSON = `{
  "xpuList": [],
  "kvmPciBridges": null
}`

func configMaps(d *simplyblockv1alpha2.SimplyblockDriver) []*corev1.ConfigMap {
	n := names(d)
	return []*corev1.ConfigMap{
		{
			ObjectMeta: metav1.ObjectMeta{Name: n.configMap, Namespace: d.Namespace},
			Data:       map[string]string{"config.json": legacyConfigJSON},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: n.nodeServerConfigMap, Namespace: d.Namespace},
			Data:       map[string]string{"nodeserver-config.json": nodeServerConfigJSON},
		},
	}
}
