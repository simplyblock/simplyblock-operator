// What the install tells the control plane about the monitoring stack, read off
// the workloads it builds.
//
// The control plane provisions Graylog, Grafana, and OpenSearch itself when the
// first storage cluster is created, and only when the management API's
// environment says monitoring is on and carries the admin password. Without
// that, the log collector ships to a Graylog with no input listening, and every
// message is lost.

package controlplane

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const testMonitoringSecret = "simplyblock-grafana-secrets"

// enabled is how both an environment flag and the log collector's annotation
// spell on.
const enabled = "true"

// A control plane with monitoring enabled tells the management API so, and gives
// it and the admin pod the admin password from the named Secret.
//
// Regression: 2026-10-06-graylog-receives-nothing — the install hard-coded
// ENABLE_MONITORING=false and carried no MONITORING_SECRET, so the first cluster
// was created with monitoring disabled, no GELF input was opened, and the log
// collector's messages were refused. The chart the install replaced set both
// from controlplane.observability.
func TestMonitoringEnabledReachesTheControlPlanesEnvironment(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.Observability = &simplyblockv1alpha2.ControlPlaneObservability{
		EnableMonitoring: true,
		SecretRef:        &corev1.LocalObjectReference{Name: testMonitoringSecret},
	}
	objects := managementAPIObjects(cp)

	api := findDeployment(t, objects, ComponentWebAPI).Spec.Template.Spec.Containers[0]
	if got := envNamed(api.Env, "ENABLE_MONITORING"); got == nil || got.Value != enabled {
		t.Errorf("management API ENABLE_MONITORING = %v, want \"true\"", got)
	}

	for _, name := range []string{ComponentWebAPI, ComponentAdminControl} {
		container := findDeployment(t, objects, name).Spec.Template.Spec.Containers[0]
		assertMonitoringSecretFrom(t, name, container.Env, testMonitoringSecret)
	}
}

// Without the block, monitoring stays off and no workload names a Secret, which
// is what a base install with no monitoring stack beside it needs.
func TestMonitoringStaysOffWithoutTheObservabilityBlock(t *testing.T) {
	objects := managementAPIObjects(localControlPlane())

	api := findDeployment(t, objects, ComponentWebAPI).Spec.Template.Spec.Containers[0]
	if got := envNamed(api.Env, "ENABLE_MONITORING"); got == nil || got.Value != "false" {
		t.Errorf("management API ENABLE_MONITORING = %v, want \"false\"", got)
	}

	for _, name := range []string{ComponentWebAPI, ComponentAdminControl} {
		container := findDeployment(t, objects, name).Spec.Template.Spec.Containers[0]
		if got := envNamed(container.Env, "MONITORING_SECRET"); got != nil {
			t.Errorf("%s carries MONITORING_SECRET with monitoring off: %v", name, got)
		}
	}
}

func assertMonitoringSecretFrom(t *testing.T, workload string, env []corev1.EnvVar, secret string) {
	t.Helper()
	got := envNamed(env, "MONITORING_SECRET")
	if got == nil {
		t.Errorf("%s carries no MONITORING_SECRET", workload)
		return
	}
	ref := got.ValueFrom
	if ref == nil || ref.SecretKeyRef == nil {
		t.Errorf("%s MONITORING_SECRET is not read from a Secret: %v", workload, got)
		return
	}
	if ref.SecretKeyRef.Name != secret || ref.SecretKeyRef.Key != "MONITORING_SECRET" {
		t.Errorf("%s MONITORING_SECRET reads %s/%s, want %s/MONITORING_SECRET",
			workload, ref.SecretKeyRef.Name, ref.SecretKeyRef.Key, secret)
	}
}

func envNamed(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

// Every process class of the database is marked for the log collector. The
// FoundationDB operator replaces general.podTemplate wholesale for a class that
// overrides it, so a mark on general alone reaches neither storage nor log.
//
// Regression: 2026-10-06-graylog-receives-nothing — no database pod was marked,
// so the processes holding every cluster definition never reached Graylog.
func TestEveryDatabaseProcessClassIsShippedToTheLogCollector(t *testing.T) {
	cluster := foundationDBCluster(localControlPlane())

	for _, class := range []string{"general", "storage", "log"} {
		annotations, _, _ := unstructured.NestedStringMap(cluster.Object,
			"spec", "processes", class, "podTemplate", "metadata", "annotations")
		if got := annotations[utils.AnnotationLogCollector]; got != enabled {
			t.Errorf("process class %s %s = %q, want \"true\"",
				class, utils.AnnotationLogCollector, got)
		}
	}
}
