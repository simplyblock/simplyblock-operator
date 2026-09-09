// Tests for the release handover's subjects. §12 is the riskiest step of the
// upgrade, because the chart that carries the new operator carries the operator
// and nothing else, so the upgrade that installs it is also the upgrade that
// deletes the running data plane. A step that showed one line where it acts on
// ninety objects is the one worst place for a plan to be vague.

package steps

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// helmOwned is the metadata Helm writes on everything it installs.
func helmOwned() map[string]string {
	return map[string]string{
		"meta.helm.sh/release-name":      "simplyblock-operator",
		"meta.helm.sh/release-namespace": "simplyblock",
	}
}

// upgradePlan is what §9.1's steps would do to this cluster.
func upgradePlan(t *testing.T, objects ...client.Object) upgrade.Plan {
	t.Helper()

	catalog := upgrade.NewCatalog()
	catalog.Steps.MustRegister(Upgrade()...)

	plan, err := upgrade.NewRunner(catalog, migration(t, objects...)).Plan(t.Context(), upgrade.StageUpgrade)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan
}

// handover returns the handover task from a plan.
func handover(t *testing.T, plan upgrade.Plan) upgrade.Task {
	t.Helper()

	for _, task := range plan.Tasks {
		if task.Step == IDHandOverRelease {
			return task
		}
	}
	t.Fatalf("the handover contributed no task:\n%v", plan.Tasks)
	return upgrade.Task{}
}

func TestHandover_NamesEveryObjectItWouldAnnotate(t *testing.T) {
	plan := upgradePlan(t,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: "simplyblock-webappapi", Namespace: "simplyblock", Annotations: helmOwned()}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
			Name: "simplyblock-csi-node", Namespace: "simplyblock", Annotations: helmOwned()}},
	)

	task := handover(t, plan)
	if len(task.Subtasks) != 2 {
		t.Fatalf("described %d subtasks, want one per object of the release:\n%v",
			len(task.Subtasks), task.Subtasks)
	}
	if !strings.Contains(task.Subtasks[0].String(), "helm.sh/resource-policy=keep") {
		t.Errorf("the subtask does not say what it would write:\n%s", task.Subtasks[0])
	}
}

func TestHandover_LeavesAnObjectNoReleaseInstalledAlone(t *testing.T) {
	// The annotation is about what Helm would prune, and Helm prunes what it
	// installed. An object nobody's release owns is not its to keep.
	plan := upgradePlan(t,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "simplyblock"}},
	)

	for _, task := range plan.Tasks {
		if task.Step == IDHandOverRelease {
			t.Fatalf("an object no Helm release installed was planned for:\n%v", task.Subtasks)
		}
	}
}

func TestHandover_LeavesAnObjectAlreadyKeptAlone(t *testing.T) {
	// The three snapshot CRDs and the snapshot controller carry the annotation
	// in their own templates already, and rewriting it would be a write with
	// nothing to change.
	kept := helmOwned()
	kept["helm.sh/resource-policy"] = "keep"

	plan := upgradePlan(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "already-kept", Namespace: "simplyblock", Annotations: kept}})

	for _, task := range plan.Tasks {
		if task.Step == IDHandOverRelease {
			t.Fatalf("an object Helm already refuses to prune was planned for:\n%v", task.Subtasks)
		}
	}
}

func TestHandover_AnObjectAlreadyKeptIsFinishedRatherThanUntouched(t *testing.T) {
	kept := helmOwned()
	kept["helm.sh/resource-policy"] = "keep"

	scope := migration(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "already-kept", Namespace: "simplyblock", Annotations: kept}})

	for _, step := range Upgrade() {
		if step.ID() != IDHandOverRelease {
			continue
		}
		covered, err := upgrade.Covered(t.Context(), scope, step)
		if err != nil {
			t.Fatalf("Covered: %v", err)
		}
		if len(covered.Finished) != 1 {
			t.Errorf("finished = %v, want the object it has nothing left to do to", covered.Finished)
		}
	}
}

func TestHandover_TheOtherUpgradeStepsStillCollapse(t *testing.T) {
	// Only the handover has objects. The rest act on the upgrade itself, and
	// printing that subject under each would say the step twice.
	plan := upgradePlan(t, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "simplyblock-webappapi", Namespace: "simplyblock", Annotations: helmOwned()}})

	for _, task := range plan.Tasks {
		if task.Step == IDHandOverRelease {
			if task.Collapsed() {
				t.Error("the handover collapsed, hiding the objects it acts on")
			}
			continue
		}
		if !task.Collapsed() {
			t.Errorf("%s did not collapse, and its only subject is the upgrade", task.Step)
		}
	}
}
