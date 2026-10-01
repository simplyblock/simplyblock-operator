// What each step builds, and the properties of it that other parts of this
// design rest on.
//
// The management API's second replica is the one worth naming. Three things
// depend on it: Degraded is defined as a component below its desired count while
// the probe still passes, a Restart recycles the workload after draining rather
// than instead of serving, and an Upgrade rolls this Deployment. With one
// instance each of those becomes an outage.

package controlplane

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Every component the phase table watches is a workload some step applies.
// A component in the table that nothing installs is one whose counts are always
// zero, which for an essential one would hold every controller in the operator
// on a workload nobody asked for.
func TestEveryWatchedComponentIsOneTheInstallApplies(t *testing.T) {
	cp := localControlPlane()
	applied := map[string]bool{}
	for _, obj := range append(foundationDBObjects(cp),
		append(datastoreObjects(cp), managementAPIObjects(cp)...)...) {
		applied[obj.GetName()] = true
	}

	for _, comp := range componentTable {
		if !applied[comp.name] {
			t.Errorf("%s is watched by the phase table and no step applies it", comp.name)
		}
	}
}

// The management API runs two instances by default, spread across machines and
// rolled one at a time with no surge. That combination is what lets a pod be
// replaced while the control plane keeps answering.
func TestTheManagementAPIRunsTwoInstancesSpreadAcrossHosts(t *testing.T) {
	cp := localControlPlane()

	api := findDeployment(t, managementAPIObjects(cp), ComponentWebAPI)

	if api.Spec.Replicas == nil || *api.Spec.Replicas != 2 {
		t.Errorf("replicas = %v, want 2: a single instance makes every restart an outage",
			api.Spec.Replicas)
	}

	affinity := api.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.PodAntiAffinity == nil ||
		len(affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) == 0 {
		t.Fatal("no required anti-affinity: two instances on one machine survive a process " +
			"crash and not the machine")
	}

	rolling := api.Spec.Strategy.RollingUpdate
	if rolling == nil {
		t.Fatal("no rolling-update strategy")
	}
	if rolling.MaxUnavailable == nil || rolling.MaxUnavailable.IntValue() != 1 {
		t.Errorf("maxUnavailable = %v, want 1", rolling.MaxUnavailable)
	}
	if rolling.MaxSurge == nil || rolling.MaxSurge.IntValue() != 0 {
		t.Errorf("maxSurge = %v, want 0", rolling.MaxSurge)
	}
}

// One instance stays expressible, because an edge deployment may prefer it. What
// it costs is stated in the design rather than prevented here, and this pins that
// the spec is honored.
func TestASingleManagementAPIInstanceStaysExpressible(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.Replicas = ptr.To(int32(1))

	api := findDeployment(t, managementAPIObjects(cp), ComponentWebAPI)

	if api.Spec.Replicas == nil || *api.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want the spec's 1", api.Spec.Replicas)
	}
}

// AdminTokenSecretRef reaches the management API as SB_ADMIN_TOKENS, sourced
// via secretKeyRef rather than a literal value, so this operator never itself
// reads the plaintext. It is what lets a cluster this control plane manages
// remotely authenticate a CreateCluster call.
func TestAnAdminTokenSecretRefReachesTheManagementAPIsEnvironment(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.AdminTokenSecretRef = &corev1.LocalObjectReference{Name: "hub-admin-token"}

	api := findDeployment(t, managementAPIObjects(cp), ComponentWebAPI)
	env := findEnvVar(t, api, "SB_ADMIN_TOKENS")

	if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("SB_ADMIN_TOKENS is not sourced from a Secret: %#v", env)
	}
	if env.ValueFrom.SecretKeyRef.Name != "hub-admin-token" {
		t.Errorf("secretKeyRef.name = %q, want %q", env.ValueFrom.SecretKeyRef.Name, "hub-admin-token")
	}
	if env.ValueFrom.SecretKeyRef.Key != "token" {
		t.Errorf("secretKeyRef.key = %q, want %q", env.ValueFrom.SecretKeyRef.Key, "token")
	}
}

// Absent names no additional credential: the management API runs exactly as
// it always has, authenticating only this operator's own service account.
func TestNoAdminTokenSecretRefMeansNoExtraEnvVar(t *testing.T) {
	cp := localControlPlane()

	api := findDeployment(t, managementAPIObjects(cp), ComponentWebAPI)

	for _, e := range api.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "SB_ADMIN_TOKENS" {
			t.Fatalf("SB_ADMIN_TOKENS set with no adminTokenSecretRef: %#v", e)
		}
	}
}

// Every workload built from the control plane's own image runs it, so an upgrade
// that writes one image onto the entity moves all of them.
func TestEveryWorkloadOfTheControlPlaneRunsTheSpecsImage(t *testing.T) {
	cp := localControlPlane()

	for _, name := range []string{
		ComponentWebAPI, ComponentTasks, ComponentMonitoring, ComponentAdminControl,
	} {
		d := findDeployment(t, managementAPIObjects(cp), name)
		for _, container := range d.Spec.Template.Spec.Containers {
			if container.Image != testImage {
				t.Errorf("%s/%s runs %q, want the spec's %q",
					name, container.Name, container.Image, testImage)
			}
		}
	}
}

// The index job runs the control plane's own backfill, on the control plane's
// image, against the database it just became able to reach. It runs to
// completion once rather than being restarted, which is what a Job is for.
func TestTheIndexJobRunsTheBackfillAgainstTheDatabase(t *testing.T) {
	job := indexJob(localControlPlane())

	spec := job.Spec.Template.Spec
	if len(spec.Containers) != 1 {
		t.Fatalf("the job runs %d containers, want the backfill alone", len(spec.Containers))
	}
	container := spec.Containers[0]

	if container.Image != testImage {
		t.Errorf("the job runs %q, want the spec's %q", container.Image, testImage)
	}
	if command := strings.Join(container.Command, " "); command != "sbctl cluster build-indices" {
		t.Errorf("the job runs %q, want the backfill command", command)
	}
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy is %q, want Never: a backfill that cannot finish is "+
			"reported rather than restarted forever", spec.RestartPolicy)
	}
	if job.Spec.BackoffLimit == nil {
		t.Fatal("no backoffLimit, so a backfill that keeps failing is restarted forever")
	}
	if *job.Spec.BackoffLimit != indexJobAttempts-1 {
		t.Errorf("backoffLimit is %d, want %d: the backfill gets %d attempts before the "+
			"install fails on it", *job.Spec.BackoffLimit, indexJobAttempts-1, indexJobAttempts)
	}
	if spec.ServiceAccountName != "" {
		t.Errorf("the job runs as %q, want no account of its own: the backfill speaks to "+
			"FoundationDB and to nothing in Kubernetes, and every account the install "+
			"creates is created after this step", spec.ServiceAccountName)
	}

	mountsClusterFile := false
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == clusterFilePath {
			mountsClusterFile = true
		}
	}
	if !mountsClusterFile {
		t.Error("the job mounts no cluster file, so it cannot reach the database")
	}
}

// The job is a client of a database that may demand a certificate of one, and
// FoundationDB's TLS is mutual: a client reaching coordinators that advertise a
// TLS listener presents its own material or it does not connect. What it
// presents is the database's peer certificate, issued by the FoundationDB step,
// rather than the management API's serving certificate, which is issued two
// steps later and would leave the pod waiting on a Secret nothing has created.
func TestTheIndexJobPresentsTheDatabasesPeerCertificate(t *testing.T) {
	job := indexJob(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))

	spec := job.Spec.Template.Spec
	container := spec.Containers[0]

	carries := map[string]bool{}
	for _, env := range container.Env {
		carries[env.Name] = true
	}
	for _, name := range []string{"FDB_TLS_CERTIFICATE_FILE", "FDB_TLS_KEY_FILE", "FDB_TLS_CA_FILE"} {
		if !carries[name] {
			t.Errorf("the job carries no %s, so it cannot authenticate to the database", name)
		}
	}

	mounted := false
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == fdbTLSMountPath {
			mounted = true
		}
	}
	if !mounted {
		t.Error("the job mounts nothing where its FDB_TLS_* variables look, so the paths " +
			"it names hold nothing")
	}

	presented := ""
	for _, volume := range spec.Volumes {
		if volume.Secret != nil {
			presented = volume.Secret.SecretName
		}
	}
	if presented != FDBPeerCertSecret {
		t.Errorf("the job presents %q, want %q: the serving certificate is issued by a later "+
			"step, so a pod naming it here never starts", presented, FDBPeerCertSecret)
	}
}

// A deployment that serves TLS without demanding a client certificate leaves the
// database's own connections plaintext, so the backfill has nothing to present
// and needs no material at all. Carrying it anyway would tie this step to a
// Secret issued for a listener the job does not run.
func TestTheIndexJobCarriesNoCertificateWhenTheDatabaseAsksForNone(t *testing.T) {
	job := indexJob(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{
		EnableMutualTLS: ptr.To(false),
	}))

	spec := job.Spec.Template.Spec
	for _, volume := range spec.Volumes {
		if volume.Secret != nil {
			t.Errorf("the job mounts Secret %q against a database that asks for no "+
				"certificate", volume.Secret.SecretName)
		}
	}
	for _, env := range spec.Containers[0].Env {
		if strings.HasPrefix(env.Name, "FDB_TLS_") {
			t.Errorf("the job carries %s against a database that asks for no certificate",
				env.Name)
		}
	}
}

// The backfill runs two steps before the management API's objects are applied,
// so every object its pod names has to be one that exists by then. Nothing about
// a name that does not resolve is loud: an account that is missing means no pod
// is ever created, a ConfigMap key holds the container in
// CreateContainerConfigError, and a Secret volume holds it in ContainerCreating.
// In all three the job stays active, carries neither the Complete nor the Failed
// condition, and the install waits on it.
func TestTheIndexJobNamesNothingTheInstallHasNotCreatedYet(t *testing.T) {
	available := map[string]bool{
		// Written by the FoundationDB operator once the database is up, which is
		// what AwaitingFoundationDB waits for.
		"ConfigMap/" + clusterFileConfigMapName: true,
		// Issued by the Certificate the ApplyingFoundationDB step applies, and
		// present for the same reason: the database mounts it itself.
		"Secret/" + FDBPeerCertSecret: true,
	}

	for _, tc := range []struct {
		name string
		tls  simplyblockv1alpha2.ControlPlaneTLS
	}{
		{"mutual TLS", simplyblockv1alpha2.ControlPlaneTLS{}},
		{"serving TLS alone", simplyblockv1alpha2.ControlPlaneTLS{EnableMutualTLS: ptr.To(false)}},
		{"plaintext", simplyblockv1alpha2.ControlPlaneTLS{EnableTLS: ptr.To(false)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := indexJob(aLocalControlPlane(tc.tls))
			for _, ref := range podReferences(job.Spec.Template.Spec) {
				if !available[ref] {
					t.Errorf("the job names %s, which no earlier step creates", ref)
				}
			}
		})
	}
}

// A job whose pods cannot be created, or are created and never start, carries
// neither the Complete nor the Failed condition: backoffLimit counts pods that
// ran and failed, and there are none. Its own wall-clock deadline is what turns
// that into the Failed condition the install stops on, rather than a step that
// holds until somebody looks at it.
func TestTheIndexJobFailsOnItsOwnDeadline(t *testing.T) {
	job := indexJob(localControlPlane())

	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("the job has no deadline, so one that cannot start is never reported failed")
	}
	if budget := time.Duration(*job.Spec.ActiveDeadlineSeconds) * time.Second; budget > buildingIndicesDeadline {
		t.Errorf("the job's deadline is %s, want it within the step's %s: a step that expires "+
			"first only reports that it is late", budget, buildingIndicesDeadline)
	}
}

// podReferences is every object a pod spec names, as "Kind/name," which is the
// set that has to exist before the pod can run.
func podReferences(spec corev1.PodSpec) []string {
	var refs []string
	if spec.ServiceAccountName != "" {
		refs = append(refs, "ServiceAccount/"+spec.ServiceAccountName)
	}

	for _, volume := range spec.Volumes {
		switch {
		case volume.ConfigMap != nil:
			refs = append(refs, "ConfigMap/"+volume.ConfigMap.Name)
		case volume.Secret != nil:
			refs = append(refs, "Secret/"+volume.Secret.SecretName)
		case volume.Projected != nil:
			for _, source := range volume.Projected.Sources {
				if source.ConfigMap != nil {
					refs = append(refs, "ConfigMap/"+source.ConfigMap.Name)
				}
				if source.Secret != nil {
					refs = append(refs, "Secret/"+source.Secret.Name)
				}
			}
		}
	}

	for _, container := range spec.Containers {
		for _, env := range container.Env {
			if env.ValueFrom == nil {
				continue
			}
			if ref := env.ValueFrom.ConfigMapKeyRef; ref != nil {
				refs = append(refs, "ConfigMap/"+ref.Name)
			}
			if ref := env.ValueFrom.SecretKeyRef; ref != nil {
				refs = append(refs, "Secret/"+ref.Name)
			}
		}
		for _, source := range container.EnvFrom {
			if source.ConfigMapRef != nil {
				refs = append(refs, "ConfigMap/"+source.ConfigMapRef.Name)
			}
			if source.SecretRef != nil {
				refs = append(refs, "Secret/"+source.SecretRef.Name)
			}
		}
	}

	return refs
}

// The exporter is deliberately not on that image: it is upstream's, and its
// version is independent of the control plane's.
func TestTheExporterDoesNotRunTheControlPlanesImage(t *testing.T) {
	cp := localControlPlane()

	exporter := findDeployment(t, managementAPIObjects(cp), ComponentFDBExporter)

	for _, container := range exporter.Spec.Template.Spec.Containers {
		if container.Image == testImage {
			t.Errorf("%s runs the control plane's image, and it is not that program",
				container.Name)
		}
	}
}

// Every service container reaches the database through the cluster file, and
// every pod that mounts it is annotated for restart when it changes. A
// coordinator change rewrites that ConfigMap, and a pod holding a connection to
// the old coordinator has to be recycled to notice.
func TestEveryWorkloadThatReachesTheDatabaseIsRolledWhenItMoves(t *testing.T) {
	cp := localControlPlane()

	for _, name := range []string{
		ComponentWebAPI, ComponentTasks, ComponentMonitoring, ComponentAdminControl,
		ComponentFDBExporter,
	} {
		d := findDeployment(t, managementAPIObjects(cp), name)

		mountsClusterFile := false
		for _, container := range d.Spec.Template.Spec.Containers {
			for _, mount := range container.VolumeMounts {
				if mount.MountPath == clusterFilePath {
					mountsClusterFile = true
				}
			}
		}
		if !mountsClusterFile {
			t.Errorf("%s mounts no cluster file, so it cannot reach the database", name)
			continue
		}
		if d.Annotations["reloader.stakater.com/configmap"] != clusterFileConfigMapName {
			t.Errorf("%s is not rolled when the cluster file changes, so a coordinator move "+
				"leaves it connected to a coordinator that is gone", name)
		}
	}
}

// The monitoring pool and the task runner are the same pod with a different
// service list, and both lists are non-empty. A pool built with no services is a
// pod that starts and does nothing, which no count would notice.
func TestTheServicePoolsRunWhatTheyDeclare(t *testing.T) {
	cp := localControlPlane()

	for _, tc := range []struct {
		name     string
		services []service
	}{
		{ComponentMonitoring, monitoringServices()},
		{ComponentTasks, taskServices()},
	} {
		if len(tc.services) == 0 {
			t.Fatalf("%s declares no services", tc.name)
		}

		d := findDeployment(t, managementAPIObjects(cp), tc.name)
		if len(d.Spec.Template.Spec.Containers) != len(tc.services) {
			t.Errorf("%s runs %d containers against %d declared services",
				tc.name, len(d.Spec.Template.Spec.Containers), len(tc.services))
		}

		seen := map[string]bool{}
		for _, container := range d.Spec.Template.Spec.Containers {
			if seen[container.Name] {
				t.Errorf("%s declares %q twice, which Kubernetes rejects", tc.name, container.Name)
			}
			seen[container.Name] = true

			if len(container.Command) != 2 || container.Command[0] != "python3" {
				t.Errorf("%s/%s runs %v, want a python3 module", tc.name, container.Name,
					container.Command)
			}
			if !strings.HasSuffix(container.Command[1], ".py") {
				t.Errorf("%s/%s runs %q, which is not a module", tc.name, container.Name,
					container.Command[1])
			}
		}
	}
}

// The control plane's account is granted exec on pods, which is the strongest
// thing in its role and the one an audit has to be able to find. Losing it would
// stop the control plane driving the storage nodes' processes, which is not a
// failure any count reports.
func TestTheControlPlanesAccountKeepsTheGrantsItCannotWorkWithout(t *testing.T) {
	cp := localControlPlane()

	role := findClusterRole(t, managementAPIObjects(cp), clusterRoleName)

	want := map[string]bool{"pods/exec": false, "tokenreviews": false, "nodes": false}
	for _, rule := range role.Rules {
		for _, resource := range rule.Resources {
			if _, tracked := want[resource]; tracked {
				want[resource] = true
			}
		}
	}
	for resource, granted := range want {
		if !granted {
			t.Errorf("the control plane's role does not grant %s", resource)
		}
	}
}

// The object store's volume comes from the same class as the database's. It
// cannot be a class this operator provides, because the control plane has to
// exist before any simplyblock volume can.
func TestTheObjectStoreTakesTheSameStorageClassAsTheDatabase(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.FoundationDB = &simplyblockv1alpha2.FoundationDBSpec{
		StorageClassName: "fast-local",
	}

	store := minioStatefulSet(cp)
	if len(store.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("the object store claims %d volumes, want 1",
			len(store.Spec.VolumeClaimTemplates))
	}
	claim := store.Spec.VolumeClaimTemplates[0]
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "fast-local" {
		t.Errorf("storageClassName = %v, want the database's fast-local",
			claim.Spec.StorageClassName)
	}
}

// Nothing the install applies names a Secret that the install does not also
// create, because a workload waiting on a Secret nobody writes never starts and
// reports the wait as a pod event rather than on the ControlPlane.
//
// "Creates" includes producing it indirectly. A cert-manager Certificate is not
// a Secret and writes one, so a workload mounting what a Certificate in the same
// install issues is waiting on something this install does produce. The set is
// read off the applied objects rather than listed here, so a certificate that
// stops being applied takes its exemption with it.
func TestNoWorkloadWaitsOnASecretTheInstallDoesNotCreate(t *testing.T) {
	cp := localControlPlane()

	objects := append(foundationDBObjects(cp),
		append(datastoreObjects(cp), managementAPIObjects(cp)...)...)
	issued := secretsTheInstallIssues(objects)

	for _, obj := range objects {
		spec := podSpecOf(obj)
		if spec == nil {
			continue
		}
		for _, volume := range spec.Volumes {
			if volume.Secret != nil && !issued[volume.Secret.SecretName] {
				t.Errorf("%s mounts Secret %q, which no step of the install creates",
					obj.GetName(), volume.Secret.SecretName)
			}
			if volume.Projected == nil {
				continue
			}
			for _, source := range volume.Projected.Sources {
				if source.Secret != nil && !issued[source.Secret.Name] {
					t.Errorf("%s projects Secret %q, which no step of the install creates",
						obj.GetName(), source.Secret.Name)
				}
			}
		}
		for _, container := range spec.Containers {
			for _, env := range container.Env {
				if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
					continue
				}
				if !issued[env.ValueFrom.SecretKeyRef.Name] {
					t.Errorf("%s/%s reads Secret %q, which no step of the install creates",
						obj.GetName(), container.Name, env.ValueFrom.SecretKeyRef.Name)
				}
			}
		}
	}
}

// secretsTheInstallIssues is the Secrets the applied objects produce without
// being one: today, what each cert-manager Certificate writes into.
func secretsTheInstallIssues(objects []client.Object) map[string]bool {
	issued := map[string]bool{}
	for _, obj := range objects {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || u.GetKind() != "Certificate" {
			continue
		}
		if name, found, _ := unstructured.NestedString(u.Object, "spec", "secretName"); found {
			issued[name] = true
		}
	}
	return issued
}

// podSpecOf is the pod template of whatever workload kind an object is, or nil
// for the objects that carry none.
func podSpecOf(obj client.Object) *corev1.PodSpec {
	switch typed := obj.(type) {
	case *appsv1.Deployment:
		return &typed.Spec.Template.Spec
	case *appsv1.StatefulSet:
		return &typed.Spec.Template.Spec
	default:
		return nil
	}
}
