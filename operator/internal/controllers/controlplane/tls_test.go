// What the install carries when a deployment asks for TLS.
//
// The assertions are on the pod spec rather than on the helpers, because the
// defect this covers was not a wrong value: it was that no value reached the
// workload at all. A helper that returns the right environment and is wired into
// three of four pod specs is the same outage as one that returns nothing.

package controlplane

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aLocalControlPlane is the singleton with the TLS block given.
func aLocalControlPlane(tls simplyblockv1alpha2.ControlPlaneTLS) *simplyblockv1alpha2.ControlPlane {
	return &simplyblockv1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonName, Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.ControlPlaneSpec{
			Source: simplyblockv1alpha2.ControlPlaneSource{
				Local: &simplyblockv1alpha2.LocalControlPlane{
					Image: "docker.io/simplyblock/simplyblock:main",
					TLS:   tls,
				},
			},
		},
	}
}

// envOf reads one variable out of a container, and says whether it was set.
func envOf(container corev1.Container, name string) (string, bool) {
	for _, e := range container.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// mountsTLS reports whether the container mounts the serving material.
func mountsTLS(container corev1.Container) bool {
	return slices.ContainsFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == tlsVolumeName && m.MountPath == tlsMountPath
	})
}

// A control plane that says nothing about TLS serves it, because the block's
// zero value is the closed one.
func TestTheInstalledControlPlaneServesTLSByDefault(t *testing.T) {
	deployment := webAPIDeployment(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))
	container := deployment.Spec.Template.Spec.Containers[0]

	if got, ok := envOf(container, "SB_TLS_SERVE"); !ok || got != "1" {
		t.Errorf("the management API was installed without a TLS listener (SB_TLS_SERVE=%q, set=%v)", got, ok)
	}
	if got, _ := envOf(container, "SB_TLS_PROVIDER"); got != string(simplyblockv1alpha2.ControlPlaneTLSCertManager) {
		t.Errorf("the issuer reached the pod as %q", got)
	}
	if got, _ := envOf(container, "SB_TLS_CLIENT_AUTH"); got != "required" {
		t.Errorf("mutual TLS is on by default and the pod asks for %q", got)
	}
	if got, _ := envOf(container, "SB_TLS_CONNECT"); got != "authenticated" {
		t.Errorf("the control plane's own calls connect as %q", got)
	}
	if !mountsTLS(container) {
		t.Error("the serving certificate is not mounted, so the listener has nothing to present")
	}
}

// Mutual TLS is the FoundationDB peer decision too, and it is the only thing
// that turns the FDB_TLS_ trio on.
func TestMutualTLSCarriesTheFoundationDBPeerFiles(t *testing.T) {
	mutual := webAPIDeployment(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))
	for _, name := range []string{"FDB_TLS_CERTIFICATE_FILE", "FDB_TLS_KEY_FILE", "FDB_TLS_CA_FILE"} {
		if _, ok := envOf(mutual.Spec.Template.Spec.Containers[0], name); !ok {
			t.Errorf("%s is not set, so FoundationDB's peers talk in the clear", name)
		}
	}

	anonymous := webAPIDeployment(aLocalControlPlane(
		simplyblockv1alpha2.ControlPlaneTLS{EnableMutualTLS: ptr.To(false)}))
	if _, ok := envOf(anonymous.Spec.Template.Spec.Containers[0], "FDB_TLS_CERTIFICATE_FILE"); ok {
		t.Error("a deployment with no client certificates still configured FoundationDB peer TLS")
	}
}

// Dropping the client certificate leaves the listener up and the caller
// anonymous, which is the narrower of the two retreats.
func TestDisablingMutualLeavesTheListenerServing(t *testing.T) {
	deployment := webAPIDeployment(aLocalControlPlane(
		simplyblockv1alpha2.ControlPlaneTLS{EnableMutualTLS: ptr.To(false)}))
	container := deployment.Spec.Template.Spec.Containers[0]

	if got, _ := envOf(container, "SB_TLS_SERVE"); got != "1" {
		t.Error("dropping the client certificate took the listener down with it")
	}
	if got, _ := envOf(container, "SB_TLS_CONNECT"); got != "anonymous" {
		t.Errorf("a deployment with no certificate to present connects as %q", got)
	}
	if _, ok := envOf(container, "SB_TLS_CLIENT_AUTH"); ok {
		t.Error("callers are still required to present a certificate")
	}
	if !mountsTLS(container) {
		t.Error("the serving certificate is no longer mounted")
	}
}

// The plaintext install is still reachable, and it carries nothing at all.
func TestDisablingServingInstallsThePlaintextControlPlane(t *testing.T) {
	deployment := webAPIDeployment(aLocalControlPlane(
		simplyblockv1alpha2.ControlPlaneTLS{EnableTLS: ptr.To(false)}))
	container := deployment.Spec.Template.Spec.Containers[0]

	for _, name := range []string{"SB_TLS_SERVE", "SB_TLS_PROVIDER", "SB_TLS_CONNECT", "SB_TLS_CLIENT_AUTH"} {
		if _, ok := envOf(container, name); ok {
			t.Errorf("a plaintext install still set %s", name)
		}
	}
	if mountsTLS(container) {
		t.Error("a plaintext install mounts a serving certificate")
	}
	if slices.ContainsFunc(deployment.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
		return v.Name == tlsVolumeName
	}) {
		t.Error("a plaintext install carries the TLS volume")
	}
}

// TestEveryControlPlaneWorkloadCarriesTheDecision is the one that matters.
//
// The management API is the server and the rest are its clients, and a client
// pool left plaintext against an API that requires a certificate cannot run the
// control plane's own tasks. Each of the four pod specs is built separately, so
// each is asserted separately.
func TestEveryControlPlaneWorkloadCarriesTheDecision(t *testing.T) {
	cp := aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{})

	for _, workload := range []struct {
		name       string
		deployment *appsv1.Deployment
	}{
		{"webappapi", webAPIDeployment(cp)},
		{"tasks", tasksDeployment(cp)},
		{"monitoring", monitoringDeployment(cp)},
		{"admin-control", adminControlDeployment(cp)},
	} {
		pod := workload.deployment.Spec.Template.Spec
		if !slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool {
			return v.Name == tlsVolumeName
		}) {
			t.Errorf("%s carries no TLS volume", workload.name)
		}
		for _, container := range pod.Containers {
			if got, _ := envOf(container, "SB_TLS_CONNECT"); got != "authenticated" {
				t.Errorf("%s/%s connects as %q", workload.name, container.Name, got)
			}
			if !mountsTLS(container) {
				t.Errorf("%s/%s mounts no certificate to present", workload.name, container.Name)
			}
		}
	}
}

// The OpenShift service CA publishes its bundle separately, so the volume is a
// projection rather than the Secret alone.
func TestTheOpenShiftIssuerProjectsItsBundle(t *testing.T) {
	deployment := webAPIDeployment(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{
		Provider: simplyblockv1alpha2.ControlPlaneTLSOpenShift,
	}))

	var volume *corev1.Volume
	for i := range deployment.Spec.Template.Spec.Volumes {
		if deployment.Spec.Template.Spec.Volumes[i].Name == tlsVolumeName {
			volume = &deployment.Spec.Template.Spec.Volumes[i]
		}
	}
	if volume == nil {
		t.Fatal("no TLS volume")
	}
	if volume.Projected == nil {
		t.Fatal("the OpenShift issuer's volume is the Secret alone, so ca.crt is missing")
	}
	if len(volume.Projected.Sources) != 2 {
		t.Fatalf("the projection carries %d sources", len(volume.Projected.Sources))
	}
}

// certificateKind is what an applied cert-manager object reports itself as.
const certificateKind = "Certificate"

// nestedAny reads a path out of the FoundationDBCluster's unstructured spec.
func nestedAny(t *testing.T, obj map[string]any, path ...string) any {
	t.Helper()
	value, found, err := unstructured.NestedFieldNoCopy(obj, path...)
	if err != nil {
		t.Fatalf("read %v: %v", path, err)
	}
	if !found {
		return nil
	}
	return value
}

// TestTheDatabaseListensForTLSWhenItsPeersAreAuthenticated is the switch, not
// the material.
//
// Every process can carry a current certificate, a key, and a CA and still talk
// to its peers in the clear: what turns the listeners over is enableTls on the
// cluster, and it is a field rather than an environment variable. A deployment
// with the mounts and without the field is the shape that looks configured and
// is not.
func TestTheDatabaseListensForTLSWhenItsPeersAreAuthenticated(t *testing.T) {
	cluster := foundationDBCluster(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))

	if got := nestedAny(t, cluster.Object, "spec", "mainContainer", "enableTls"); got != true {
		t.Errorf("the database's own listeners carry enableTls = %v", got)
	}
	if got := nestedAny(t, cluster.Object, "spec", "sidecarContainer", "enableTls"); got != true {
		t.Errorf("the sidecar carries enableTls = %v", got)
	}
}

// Every process class carries the certificate, because the FoundationDB operator
// replaces general.podTemplate wholesale for a class that overrides it: a volume
// stated once on general reaches neither storage nor log.
func TestEveryProcessClassCarriesThePeerCertificate(t *testing.T) {
	cluster := foundationDBCluster(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))

	for _, class := range []string{"general", "storage", "log"} {
		volumes := nestedAny(t, cluster.Object,
			"spec", "processes", class, "podTemplate", "spec", "volumes")
		if volumes == nil {
			t.Errorf("process class %s carries no peer certificate volume", class)
			continue
		}
		if len(volumes.([]any)) != 1 {
			t.Errorf("process class %s carries %d volumes", class, len(volumes.([]any)))
		}

		containers := nestedAny(t, cluster.Object,
			"spec", "processes", class, "podTemplate", "spec", "containers")
		first := containers.([]any)[0].(map[string]any)
		if first["env"] == nil {
			t.Errorf("process class %s has no FDB_TLS_ environment", class)
		}
		if first["volumeMounts"] == nil {
			t.Errorf("process class %s mounts nothing to read the certificate from", class)
		}
	}
}

// A deployment whose callers are anonymous has no peer TLS either. There is no
// anonymous mode between database processes: each authenticates to the others or
// none of them do.
func TestAnAnonymousDeploymentLeavesTheDatabaseInTheClear(t *testing.T) {
	cluster := foundationDBCluster(aLocalControlPlane(
		simplyblockv1alpha2.ControlPlaneTLS{EnableMutualTLS: ptr.To(false)}))

	if got := nestedAny(t, cluster.Object, "spec", "mainContainer", "enableTls"); got != nil {
		t.Errorf("the database listens for TLS with no certificates issued for it (%v)", got)
	}
	for _, class := range []string{"general", "storage", "log"} {
		if got := nestedAny(t, cluster.Object,
			"spec", "processes", class, "podTemplate", "spec", "volumes"); got != nil {
			t.Errorf("process class %s carries a certificate volume it has no use for", class)
		}
	}
}

// The FoundationDB operator reconciles the database, so it reaches it the way
// its processes reach each other.
func TestTheDatabaseOperatorCarriesThePeerCertificate(t *testing.T) {
	deployment := fdbOperatorDeployment(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))
	manager := deployment.Spec.Template.Spec.Containers[0]

	if _, ok := envOf(manager, "FDB_TLS_CERTIFICATE_FILE"); !ok {
		t.Error("the database operator has no certificate, so it cannot reach a TLS cluster")
	}
	if !slices.ContainsFunc(manager.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.MountPath == fdbTLSMountPath
	}) {
		t.Error("the database operator mounts nothing at the path its environment names")
	}
	if !slices.ContainsFunc(deployment.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
		return v.Secret != nil && v.Secret.SecretName == FDBPeerCertSecret
	}) {
		t.Error("the database operator's pod carries no peer certificate volume")
	}
}

// The peer certificate is issued once, by whoever installs the database.
//
// Regression: 2026-09-21-two-certificates-one-secret — the install applied a
// second Certificate for simplyblock-foundationdb-tls beside the chart's, under
// a different name and with only the usages a server needs. Two controllers
// issuing into one Secret is a certificate that changes whenever either of them
// writes, and the narrower one takes client auth away from a database whose
// processes are each other's clients.
func TestThePeerCertificateIsIssuedOnceAndForBothRoles(t *testing.T) {
	objects := foundationDBObjects(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))

	var issued []*unstructured.Unstructured
	for _, obj := range objects {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || u.GetKind() != certificateKind {
			continue
		}
		name, _, _ := unstructured.NestedString(u.Object, "spec", "secretName")
		if name == FDBPeerCertSecret {
			issued = append(issued, u)
		}
	}

	if len(issued) != 1 {
		t.Fatalf("%d certificates issue %s", len(issued), FDBPeerCertSecret)
	}
	if got := issued[0].GetName(); got != fdbPeerCertificateName {
		t.Errorf("the peer certificate is named %q, and the chart's Secret is claimed by %q",
			got, fdbPeerCertificateName)
	}

	usages, _, _ := unstructured.NestedStringSlice(issued[0].Object, "spec", "usages")
	for _, want := range []string{"server auth", "client auth"} {
		if !slices.Contains(usages, want) {
			t.Errorf("the peer certificate is missing %q, and every process is both", want)
		}
	}
}

// A deployment with no peer TLS issues nothing for it.
func TestNoPeerCertificateWithoutPeerTLS(t *testing.T) {
	objects := foundationDBObjects(aLocalControlPlane(
		simplyblockv1alpha2.ControlPlaneTLS{EnableMutualTLS: ptr.To(false)}))

	for _, obj := range objects {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == certificateKind {
			t.Errorf("a deployment with no peer TLS issues %s", u.GetName())
		}
	}
}
