// Every name the SimplyblockDriver's deployment uses, derived in one place.
//
// The derivation is `<object name>-csi-<component>`, and the chart writes the same
// strings literally, so a SimplyblockDriver named "simplyblock" lands on the
// objects a chart install left behind and adoption is a Get on each rather than
// a mapping table maintained against chart history. names_test.go is what holds
// the two sides together.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.3.

package driver

import (
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// clusterRoleComponents are the five ClusterRole and ClusterRoleBinding pairs
// the plugins need: one for the node plugin, and one per controller-plugin
// sidecar that talks to the API server.
var clusterRoleComponents = []string{
	nodeComponent, "provisioner", "attacher", "resizer", "health-monitor",
}

// nodeComponent is the one component whose role binds the node plugin's account
// rather than the controller plugin's.
const nodeComponent = "node"

// objectNames is the whole naming surface of one deployment.
type objectNames struct {
	prefix string

	nodeDaemonSet            string
	controllerStatefulSet    string
	nodeServiceAccount       string
	controllerServiceAccount string
	configMap                string
	nodeServerConfigMap      string
	secret                   string
	secretV2                 string
	snapshotClass            string

	// csiDriver is spec.driverName rather than a derived string. It is the name
	// every PersistentVolume records in spec.csi.driver, so it is the cluster's
	// to know and not this object's to pick.
	csiDriver string
}

func names(d *simplyblockv1alpha2.SimplyblockDriver) objectNames {
	p := d.Name + "-csi-"
	return objectNames{
		prefix:                   p,
		nodeDaemonSet:            p + "node",
		controllerStatefulSet:    p + "controller",
		nodeServiceAccount:       p + "node-sa",
		controllerServiceAccount: p + "controller-sa",
		configMap:                p + "cm",
		nodeServerConfigMap:      p + "nodeservercm",
		secret:                   p + "secret",
		secretV2:                 p + "secret-v2",
		snapshotClass:            p + "snapshotclass",
		csiDriver:                driverName(d),
	}
}

func (n objectNames) clusterRole(component string) string {
	return n.prefix + component + "-role"
}

func (n objectNames) clusterRoleBinding(component string) string {
	return n.prefix + component + "-binding"
}

// driverName is spec.driverName with the CRD's default applied, so that code
// reading it does not have to care whether admission had a chance to default it.
func driverName(d *simplyblockv1alpha2.SimplyblockDriver) string {
	if d.Spec.DriverName != "" {
		return d.Spec.DriverName
	}
	return DefaultDriverName
}

// DefaultDriverName is the CRD's default for spec.driverName, and the name the
// chart registers under.
const DefaultDriverName = "csi.simplyblock.io"
