// The tests of the cluster-file stamp: that a coordinator move reaches the
// workloads holding a connection to the old coordinators without a third-party
// reloader, and that nothing else rolls them.

package controlplane

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// The literal is deliberate: the test pins the name customers and tooling see,
// and not the constant that happens to hold it.
const stampAnnotation = "storage.simplyblock.io/cluster-file-hash"

// clusterFileWorkloads are the Deployments that mount the cluster file.
var clusterFileWorkloads = []string{
	ComponentWebAPI, ComponentTasks, ComponentMonitoring, ComponentAdminControl, ComponentFDBExporter,
}

func clusterFileConfigMap(data string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: clusterFileConfigMapName, Namespace: testNamespace},
		Data:       map[string]string{clusterFileKey: data},
	}
}

func stampOf(t *testing.T, c client.Client, name string) string {
	t.Helper()
	var d appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &d); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return d.Spec.Template.Annotations[stampAnnotation]
}

// Regression: 2026-10-29-drop-reloader — the pods that mount the cluster file
// were rolled by a third-party controller with cluster-wide Secret read and
// workload patch rights. The operator rolls them itself, by the file's hash.
func TestEveryWorkloadMountingTheClusterFileCarriesItsHash(t *testing.T) {
	cp := localControlPlane()
	c := newClient(t, cp, clusterFileConfigMap("sb:abc@10.0.0.1:4500"))
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.applyEverything(context.Background(), cp); err != nil {
		t.Fatalf("apply: %v", err)
	}

	for _, name := range clusterFileWorkloads {
		if stampOf(t, c, name) == "" {
			t.Errorf("%s mounts the cluster file and carries no hash of it, so a coordinator move "+
				"leaves it connected to a coordinator that is gone", name)
		}
	}
}

// Regression: 2026-10-29-drop-reloader.
func TestAChangedClusterFileChangesTheStampAndAnUnchangedOneDoesNot(t *testing.T) {
	ctx := context.Background()
	cp := localControlPlane()
	cm := clusterFileConfigMap("sb:abc@10.0.0.1:4500")
	c := newClient(t, cp, cm)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first := stampOf(t, c, ComponentWebAPI)
	if first == "" {
		t.Fatal("the first apply stamped nothing")
	}

	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if got := stampOf(t, c, ComponentWebAPI); got != first {
		t.Errorf("stamp = %q after an apply with the file unchanged, want %q: a steady state would roll the pods", got, first)
	}

	cm.Data[clusterFileKey] = "sb:abc@10.0.0.9:4500"
	if err := c.Update(ctx, cm); err != nil {
		t.Fatalf("move the coordinator: %v", err)
	}
	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if got := stampOf(t, c, ComponentWebAPI); got == first {
		t.Errorf("stamp is still %q after the coordinators moved, so the pods are not rolled", got)
	}
}

// The file is written by the FoundationDB operator, so it can be absent. An
// install that waits for it would deadlock on the step that applies the workloads
// the database's clients are among.
func TestAMissingClusterFileIsNotAnErrorAndStampsNothing(t *testing.T) {
	cp := localControlPlane()
	c := newClient(t, cp)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.applyEverything(context.Background(), cp); err != nil {
		t.Fatalf("apply without the file: %v", err)
	}
	for _, name := range clusterFileWorkloads {
		if got := stampOf(t, c, name); got != "" {
			t.Errorf("%s carries the stamp %q with no file to hash", name, got)
		}
	}
}

// Regression: 2026-10-30-stamp-retention — deleting the file made the next
// apply strip the stamp, which rolled every workload to an unstamped template,
// and the file's return rolled them all again.
func TestAFileThatVanishesLeavesTheStampItsWorkloadsCarry(t *testing.T) {
	ctx := context.Background()
	cp := localControlPlane()
	cm := clusterFileConfigMap("sb:abc@10.0.0.1:4500")
	c := newClient(t, cp, cm)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	stamped := stampOf(t, c, ComponentWebAPI)
	if stamped == "" {
		t.Fatal("the first apply stamped nothing")
	}

	if err := c.Delete(ctx, cm); err != nil {
		t.Fatalf("delete the file: %v", err)
	}
	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply without the file: %v", err)
	}
	for _, name := range clusterFileWorkloads {
		if got := stampOf(t, c, name); got != stamped {
			t.Errorf("%s carries stamp %q after the file vanished, want %q kept: stripping it rolls the pods "+
				"to an unstamped template and the file's return rolls them again", name, got, stamped)
		}
	}

	back := clusterFileConfigMap("sb:abc@10.0.0.1:4500")
	if err := c.Create(ctx, back); err != nil {
		t.Fatalf("recreate the file: %v", err)
	}
	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply with the file back: %v", err)
	}
	if got := stampOf(t, c, ComponentWebAPI); got != stamped {
		t.Errorf("stamp = %q after the same file returned, want %q", got, stamped)
	}
}

// Regression: 2026-10-29-drop-reloader — the reloader's annotations were the
// contract with a controller that no longer ships.
func TestNoWorkloadCarriesAReloaderAnnotation(t *testing.T) {
	cp := localControlPlane()
	c := newClient(t, cp, clusterFileConfigMap("sb:abc@10.0.0.1:4500"))
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.applyEverything(context.Background(), cp); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var deployments appsv1.DeploymentList
	if err := c.List(context.Background(), &deployments, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, d := range deployments.Items {
		for _, annotations := range []map[string]string{d.Annotations, d.Spec.Template.Annotations} {
			for key := range annotations {
				if strings.HasPrefix(key, "reloader.stakater.com/") {
					t.Errorf("%s carries %s, which nothing acts on", d.Name, key)
				}
			}
		}
	}
}

func TestAClusterFileChangeWakesTheControlPlaneInItsNamespaceOnly(t *testing.T) {
	cp := localControlPlane()
	c := newClient(t, cp)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	got := r.clusterFileRequests(context.Background(), clusterFileConfigMap("x"))
	want := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cp)}
	if len(got) != 1 || got[0] != want {
		t.Errorf("requests = %v, want exactly %v", got, want)
	}

	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: testNamespace}}
	if got := r.clusterFileRequests(context.Background(), other); len(got) != 0 {
		t.Errorf("an unrelated ConfigMap woke %v", got)
	}

	elsewhere := clusterFileConfigMap("x")
	elsewhere.Namespace = "elsewhere"
	if got := r.clusterFileRequests(context.Background(), elsewhere); len(got) != 0 {
		t.Errorf("a ConfigMap in another namespace woke %v", got)
	}
}
