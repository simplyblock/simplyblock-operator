// The derivation has to reproduce the names the chart writes, because that is
// what lets adoption be a Get on each object rather than a mapping table
// maintained against chart history.
//
// The literals below are measured from a live 26.2.7 release rather than read
// back out of the derivation, so a change to either side breaks this file.

package driver

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// chartNames is what `helm-charts/charts/simplyblock-operator` applies today,
// and what a SimplyblockDriver named "simplyblock" must therefore derive.
var chartNames = map[string]string{
	"node daemonset":         "simplyblock-csi-node",
	"controller statefulset": "simplyblock-csi-controller",
	"node sa":                "simplyblock-csi-node-sa",
	"controller sa":          "simplyblock-csi-controller-sa",
	"config configmap":       "simplyblock-csi-cm",
	"nodeserver configmap":   "simplyblock-csi-nodeservercm",
	"secret":                 "simplyblock-csi-secret",
	"secret v2":              "simplyblock-csi-secret-v2",
	"snapshot class":         "simplyblock-csi-snapshotclass",
	"node role":              "simplyblock-csi-node-role",
	"node binding":           "simplyblock-csi-node-binding",
	"provisioner role":       "simplyblock-csi-provisioner-role",
	"provisioner binding":    "simplyblock-csi-provisioner-binding",
	"attacher role":          "simplyblock-csi-attacher-role",
	"attacher binding":       "simplyblock-csi-attacher-binding",
	"resizer role":           "simplyblock-csi-resizer-role",
	"resizer binding":        "simplyblock-csi-resizer-binding",
	"health-monitor role":    "simplyblock-csi-health-monitor-role",
	"health-monitor binding": "simplyblock-csi-health-monitor-binding",
}

func testDriver(name string) *simplyblockv1alpha2.SimplyblockDriver {
	return &simplyblockv1alpha2.SimplyblockDriver{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.SimplyblockDriverSpec{
			Image:      "quay.io/simplyblock-io/spdkcsi:v26.2.6",
			DriverName: "csi.simplyblock.io",
		},
	}
}

func derivedNames(d *simplyblockv1alpha2.SimplyblockDriver) map[string]string {
	n := names(d)
	out := map[string]string{
		"node daemonset":         n.nodeDaemonSet,
		"controller statefulset": n.controllerStatefulSet,
		"node sa":                n.nodeServiceAccount,
		"controller sa":          n.controllerServiceAccount,
		"config configmap":       n.configMap,
		"nodeserver configmap":   n.nodeServerConfigMap,
		"secret":                 n.secret,
		"secret v2":              n.secretV2,
		"snapshot class":         n.snapshotClass,
	}
	for _, r := range clusterRoleComponents {
		out[r+" role"] = n.clusterRole(r)
		out[r+" binding"] = n.clusterRoleBinding(r)
	}
	return out
}

// U-59: the derivation lands on the objects a chart install left behind.
func TestNamesReproduceTheChart(t *testing.T) {
	got := derivedNames(testDriver("simplyblock"))

	for what, want := range chartNames {
		if got[what] != want {
			t.Errorf("%s = %q, want the chart's %q", what, got[what], want)
		}
	}
	for what, name := range got {
		if _, expected := chartNames[what]; !expected {
			t.Errorf("derived an object the chart list does not cover: %s = %q", what, name)
		}
	}
}

// U-60: a differently named driver derives a set that overlaps the first
// nowhere, so it finds nothing to adopt and contends over nothing.
func TestNamesOfASecondDriverAreDisjoint(t *testing.T) {
	first := derivedNames(testDriver("simplyblock"))
	second := derivedNames(testDriver("tenant-b"))

	taken := make(map[string]string, len(first))
	for what, name := range first {
		taken[name] = what
	}
	for what, name := range second {
		if other, clash := taken[name]; clash {
			t.Errorf("%s derived %q, which the first driver's %s already holds", what, name, other)
		}
	}
}

// The registration is the one name that is not derived, because it is the name
// the cluster provisions through rather than one this object gets to pick.
func TestRegistrationIsTheDriverName(t *testing.T) {
	d := testDriver("tenant-b")
	if got := names(d).csiDriver; got != "csi.simplyblock.io" {
		t.Errorf("registration = %q, want spec.driverName", got)
	}
}
