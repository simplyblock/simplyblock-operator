// The installation machine: what each step applies, what it holds on, and the
// two properties the whole thing rests on.
//
// The first is that every step is an apply, so re-entering one is a no-op. That
// is what lets the machine carry no triggered flag, and it is why an install
// interrupted anywhere resumes rather than restarts.
//
// The second is that a held step reports what it is waiting for, read from the
// thing being waited on. An install stalled on FoundationDB names the database
// rather than the operator, which is the difference between a message somebody
// can act on and one that sends them to the logs.

package controlplane

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The graph is a line from the first apply to the readiness wait, and every step
// declares a successor except the last. The reconciler takes the first edge as
// the only edge, which holds exactly while that is true.
func TestTheInstallationGraphIsALine(t *testing.T) {
	graph := installGraph()

	if graph.Initial != stepApplyingFoundationDB {
		t.Errorf("the install starts at %s, want %s", graph.Initial, stepApplyingFoundationDB)
	}

	want := map[installStep]installStep{
		stepApplyingFoundationDB: stepAwaitingFoundationDB,
		stepAwaitingFoundationDB: stepApplyingDatastore,
		stepApplyingDatastore:    stepApplyingAPI,
		stepApplyingAPI:          stepAwaitingAPI,
	}
	for from, to := range want {
		state, declared := graph.States[from]
		if !declared {
			t.Fatalf("%s is not a state of the graph", from)
		}
		if len(state.To) != 1 || state.To[0] != to {
			t.Errorf("%s declares successors %v, want exactly [%s]", from, state.To, to)
		}
	}

	if last := graph.States[stepAwaitingAPI]; len(last.To) != 0 {
		t.Errorf("%s declares successors %v, want none: reaching it is what makes the "+
			"control plane Available", stepAwaitingAPI, last.To)
	}
}

// Every step has a budget, and the machine is built from the same table the
// reconciler sets the first step's deadline from, so every step can time out.
func TestEveryInstallationStepHasABudget(t *testing.T) {
	for step := range installGraph().States {
		if _, ok := installStepBudgets[step]; !ok {
			t.Errorf("%s has no budget, so a deadline is never set for it", step)
		}
	}
	for step := range installStepBudgets {
		if _, ok := installGraph().States[step]; !ok {
			t.Errorf("%s has a budget and is not a state of the graph", step)
		}
	}
}

// The step enum the API admits and the states the graph declares are the same
// set. They are two lists in two files, and a value added to one and not the
// other is a status the API rejects or a step nothing can reach.
func TestTheStepEnumAndTheGraphAgree(t *testing.T) {
	declared := map[string]bool{}
	for _, state := range statemachine.DeclaredStates(installGraph()) {
		declared[state] = true
	}

	admitted := []installStep{
		simplyblockv1alpha2.ControlPlaneStepApplyingFoundationDB,
		simplyblockv1alpha2.ControlPlaneStepAwaitingFoundationDB,
		simplyblockv1alpha2.ControlPlaneStepApplyingDatastore,
		simplyblockv1alpha2.ControlPlaneStepApplyingAPI,
		simplyblockv1alpha2.ControlPlaneStepAwaitingAPI,
	}
	if len(declared) != len(admitted) {
		t.Errorf("the graph declares %d states and the enum admits %d", len(declared), len(admitted))
	}
	for _, step := range admitted {
		if !declared[string(step)] {
			t.Errorf("the enum admits %s and no graph state declares it", step)
		}
	}
}

// Re-applying a step puts back what somebody deleted and corrects what somebody
// edited, which is the property steady state rests on and the reason no
// operation exists for checking the install. It is also what makes re-entering a
// step a no-op, and therefore why the machine carries no triggered flag.
func TestReApplyingAStepCorrectsWhatWasChangedUnderIt(t *testing.T) {
	ctx := context.Background()
	cp := localControlPlane()
	c := newClient(t, cp)
	scheme := testScheme(t)

	if err := applyAll(ctx, c, cp, scheme, managementAPIObjects(cp)); err != nil {
		t.Fatalf("the first apply: %v", err)
	}

	// Somebody scales the management API to one replica and deletes its Service.
	var api appsv1.Deployment
	key := client.ObjectKey{Namespace: testNamespace, Name: ComponentWebAPI}
	if err := c.Get(ctx, key, &api); err != nil {
		t.Fatalf("read the management API: %v", err)
	}
	api.Spec.Replicas = ptr.To(int32(1))
	if err := c.Update(ctx, &api); err != nil {
		t.Fatalf("scale the management API down: %v", err)
	}
	if err := c.Delete(ctx, webAPIService(cp)); err != nil {
		t.Fatalf("delete the Service: %v", err)
	}

	if err := applyAll(ctx, c, cp, scheme, managementAPIObjects(cp)); err != nil {
		t.Fatalf("the second apply: %v", err)
	}

	if err := c.Get(ctx, key, &api); err != nil {
		t.Fatalf("read the management API back: %v", err)
	}
	if api.Spec.Replicas == nil || *api.Spec.Replicas != 2 {
		t.Errorf("replicas = %v after re-applying, want the spec's 2", api.Spec.Replicas)
	}

	var service corev1.Service
	if err := c.Get(ctx, key, &service); err != nil {
		t.Errorf("the Service was not put back: %v", err)
	}

	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list the deployments: %v", err)
	}
	if len(deployments.Items) != 5 {
		names := make([]string, 0, len(deployments.Items))
		for _, d := range deployments.Items {
			names = append(names, d.Name)
		}
		t.Errorf("two passes produced %d deployments (%v), want the same 5 both times",
			len(deployments.Items), names)
	}
}

// A namespaced object is a child of the ControlPlane and goes with the garbage
// collector. A cluster-scoped one cannot be, because Kubernetes never collects a
// cluster-scoped object owned by a namespaced one, so it carries the managed-by
// label the finalizer deletes on instead.
func TestOwnershipFollowsTheObjectsScope(t *testing.T) {
	cp := localControlPlane()
	scheme := testScheme(t)

	for _, obj := range append(foundationDBObjects(cp), managementAPIObjects(cp)...) {
		if err := setOwnership(cp, obj, scheme); err != nil {
			t.Fatalf("set ownership on %s: %v", obj.GetName(), err)
		}

		if obj.GetNamespace() != "" {
			if len(obj.GetOwnerReferences()) == 0 {
				t.Errorf("%s is namespaced and carries no owner reference, so deleting the "+
					"ControlPlane would leave it behind", obj.GetName())
			}
			continue
		}

		if !mayDelete(obj) {
			t.Errorf("%s is cluster-scoped and carries no managed-by label, so the finalizer "+
				"would leave it behind", obj.GetName())
		}
		if len(obj.GetOwnerReferences()) != 0 {
			t.Errorf("%s is cluster-scoped and carries an owner reference to a namespaced "+
				"object, which Kubernetes never resolves and never collects", obj.GetName())
		}
	}
}

// The finalizer deletes only what this controller marked. A cluster-scoped
// object is shared ground, and deleting one carrying another controller's mark,
// or none, takes away somebody else's RBAC.
func TestTheFinalizerLeavesUnmarkedClusterScopedObjectsAlone(t *testing.T) {
	somebodyElses := fdbManagerClusterRoleObject()
	somebodyElses.Labels = map[string]string{managedByLabel: "somebody-else"}

	c := newClient(t, somebodyElses)

	if err := deleteIfMarked(context.Background(), c, fdbManagerClusterRoleObject()); err != nil {
		t.Fatalf("deleteIfMarked: %v", err)
	}

	var survived rbacv1.ClusterRole
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: somebodyElses.Name}, &survived); err != nil {
		t.Errorf("the ClusterRole was deleted, and this controller never marked it: %v", err)
	}
}

// AwaitingFoundationDB reports what it is waiting for, read from the database's
// own status, so a stalled install names the coordinator rather than the
// operator.
func TestAwaitingFoundationDBNamesWhatItIsWaitingOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health fdbHealth
		want   string
	}{
		{
			name:   "the cluster has not been created",
			health: fdbHealth{},
			want:   "has not been created yet",
		},
		{
			name:   "the cluster is not available",
			health: fdbHealth{found: true, desired: 7, reconciled: 3},
			want:   "has 3 of 7 process groups reconciled",
		},
		{
			name:   "the cluster is available and not fully replicated",
			health: fdbHealth{found: true, available: true, desired: 7, reconciled: 7},
			want:   "not yet fully replicated",
		},
		{
			name: "the cluster is ready",
			health: fdbHealth{
				found: true, available: true, fullReplication: true, desired: 7, reconciled: 7,
			},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.health.waitingOn()
			switch {
			case tc.want == "" && got != "":
				t.Errorf("waitingOn = %q, want nothing: the step is finished", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("waitingOn = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// A FoundationDBCluster that is not there is not an error. The apply created it
// and the cache has not caught up, which is the ordinary state on the pass right
// after ApplyingFoundationDB.
func TestReadingAnAbsentFoundationDBClusterIsNotAnError(t *testing.T) {
	c := newClient(t)

	health, err := readFoundationDB(context.Background(), c, testNamespace)
	if err != nil {
		t.Fatalf("readFoundationDB: %v", err)
	}
	if health.found {
		t.Error("a cluster that does not exist was reported as found")
	}
}

// AwaitingAPI holds on the probe rather than on the pod counts, because the
// question it answers is whether the control plane can be reached at all.
func TestAwaitingAPIHoldsUntilTheProbePasses(t *testing.T) {
	cp := localControlPlane()
	prober := &stubProber{ready: false, readyMessage: "connection refused"}
	r := &ControlPlaneReconciler{
		Client: newClient(t, cp), Scheme: testScheme(t), Prober: prober,
	}

	done, held, err := r.performInstallStep(context.Background(), cp, stepAwaitingAPI)
	if err != nil {
		t.Fatalf("performInstallStep: %v", err)
	}
	if done {
		t.Fatal("the step finished while the probe was failing")
	}
	if !strings.Contains(held, "connection refused") {
		t.Errorf("held on %q, want the probe's own words", held)
	}

	prober.ready = true
	done, _, err = r.performInstallStep(context.Background(), cp, stepAwaitingAPI)
	if err != nil {
		t.Fatalf("performInstallStep: %v", err)
	}
	if !done {
		t.Error("the step held while the probe was passing")
	}
}

// The FoundationDBCluster is built with the coordinator count and the redundancy
// mode agreeing, so the database is never told to keep more copies than it has
// processes to keep them on.
func TestTheCoordinatorCountAndTheRedundancyModeAgree(t *testing.T) {
	for _, tc := range []struct {
		replicas int32
		mode     string
	}{
		{1, "single"},
		{3, "double"},
		{5, "triple"},
		{7, "triple"},
	} {
		cp := localControlPlane()
		cp.Spec.Source.Local.FoundationDB = &simplyblockv1alpha2.FoundationDBSpec{
			Replicas: &tc.replicas,
		}

		cluster := foundationDBCluster(cp)
		mode, _, _ := unstructured.NestedString(cluster.Object,
			"spec", "databaseConfiguration", "redundancy_mode")
		logs, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "processCounts", "log")

		if mode != tc.mode {
			t.Errorf("%d coordinators: redundancy_mode = %q, want %q", tc.replicas, mode, tc.mode)
		}
		if logs != int64(tc.replicas) {
			t.Errorf("%d coordinators: processCounts.log = %d, want %d", tc.replicas, logs, tc.replicas)
		}
	}
}

// An unset storage class leaves the field out rather than writing an empty
// string, which is the difference between the cluster's default class and a
// class literally named nothing.
func TestAnUnsetStorageClassIsAbsentRatherThanEmpty(t *testing.T) {
	cp := localControlPlane()

	claim := volumeClaimSpec(foundationDBSpecOf(cp))

	if _, present := claim["storageClassName"]; present {
		t.Error("storageClassName is written for a spec that states none")
	}

	cp.Spec.Source.Local.FoundationDB = &simplyblockv1alpha2.FoundationDBSpec{
		StorageClassName: "fast",
	}
	claim = volumeClaimSpec(foundationDBSpecOf(cp))
	if claim["storageClassName"] != "fast" {
		t.Errorf("storageClassName = %v, want fast", claim["storageClassName"])
	}
}

// The scheduling an operator set reaches every pod the install creates, so the
// control plane stays where the deployment put it.
func TestSchedulingReachesEveryPodTheInstallCreates(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.NodeSelector = map[string]string{"simplyblock.io/control-plane": "true"}
	cp.Spec.Source.Local.Tolerations = []corev1.Toleration{{
		Key: "simplyblock.io/dedicated", Operator: corev1.TolerationOpExists,
	}}

	for _, obj := range append(foundationDBObjects(cp), append(datastoreObjects(cp), managementAPIObjects(cp)...)...) {
		var spec *corev1.PodSpec
		switch typed := obj.(type) {
		case *appsv1.Deployment:
			spec = &typed.Spec.Template.Spec
		case *appsv1.StatefulSet:
			spec = &typed.Spec.Template.Spec
		default:
			continue
		}

		if len(spec.NodeSelector) == 0 {
			t.Errorf("%s carries no node selector", obj.GetName())
		}
		if len(spec.Tolerations) == 0 {
			t.Errorf("%s carries no tolerations", obj.GetName())
		}
	}
}
