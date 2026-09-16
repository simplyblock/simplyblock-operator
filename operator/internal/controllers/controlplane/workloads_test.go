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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

// Every workload built from the control plane's own image runs it, so an upgrade
// that writes one image onto the entity moves all of them. A workload left on a
// hard-coded image would be the one the upgrade did not reach.
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

// The exporter is deliberately not on that image: it is upstream's, and pinning
// it to the control plane's would mean an upgrade replacing a binary that has
// nothing to do with the control plane's version.
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
func TestNoWorkloadWaitsOnASecretTheInstallDoesNotCreate(t *testing.T) {
	cp := localControlPlane()

	for _, obj := range append(foundationDBObjects(cp),
		append(datastoreObjects(cp), managementAPIObjects(cp)...)...) {
		spec := podSpecOf(obj)
		if spec == nil {
			continue
		}
		for _, volume := range spec.Volumes {
			if volume.Secret != nil {
				t.Errorf("%s mounts Secret %q, which no step of the install creates",
					obj.GetName(), volume.Secret.SecretName)
			}
		}
		for _, container := range spec.Containers {
			for _, env := range container.Env {
				if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
					t.Errorf("%s/%s reads Secret %q, which no step of the install creates",
						obj.GetName(), container.Name, env.ValueFrom.SecretKeyRef.Name)
				}
			}
		}
	}
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
