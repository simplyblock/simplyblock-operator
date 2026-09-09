// Tests for §16's steps. The point they turn on is that describing a change and
// performing it need different things: a step whose target type §29.1 has not
// written still says, object by object, what it would do.

package steps

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// migratePlan is what §16's steps would do to this cluster.
func migratePlan(t *testing.T, objects ...client.Object) upgrade.Plan {
	t.Helper()

	catalog := upgrade.NewCatalog()
	catalog.Steps.MustRegister(Migrate()...)

	plan, err := upgrade.NewRunner(catalog, migration(t, objects...)).Plan(t.Context(), upgrade.StageMigrate)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan
}

// taskFor returns one step's task from a plan.
func taskFor(t *testing.T, plan upgrade.Plan, id upgrade.ID) upgrade.Task {
	t.Helper()

	for _, task := range plan.Tasks {
		if task.Step == id {
			return task
		}
	}
	t.Fatalf("%s contributed no task:\n%v", id, plan.Tasks)
	return upgrade.Task{}
}

// claim carries whatever annotations a test needs.
func claim(namespace, name string, annotations map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: annotations},
	}
}

// legacyPV is a PersistentVolume whose handle carries a pool name.
func legacyPV(name, handle string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "csi.simplyblock.io", VolumeHandle: handle,
			}}},
	}
}

const (
	legacyHandleValue = "2f4f0300-9993-4289-be95-59414fc8a54d:my-pool:8b1f0300-9993-4289-be95-59414fc8a54d"
	modernHandleValue = "2f4f0300-9993-4289-be95-59414fc8a54d:1c2c0300-9993-4289-be95-59414fc8a54d:8b1f0300-9993-4289-be95-59414fc8a54d"
)

func TestMigrate_AnUnimplementedStepStillNamesEveryObject(t *testing.T) {
	// The whole point of splitting Describe from Apply. Saying that a
	// BackupPolicy becomes a StorageBackupPolicy needs the BackupPolicy, which
	// discovery has, and not the target type, which §29.1 has not written.
	plan := migratePlan(t,
		&simplyblockv1alpha1.BackupPolicy{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "simplyblock"}},
		&simplyblockv1alpha1.BackupPolicy{ObjectMeta: metav1.ObjectMeta{Name: "hourly", Namespace: "simplyblock"}},
	)

	task := taskFor(t, plan, IDCopyBackupPolicies)
	if task.Blocked == "" {
		t.Error("the task is not marked, and it cannot be performed")
	}
	if len(task.Subtasks) != 2 {
		t.Fatalf("described %d subtasks, want one per policy:\n%v", len(task.Subtasks), task.Subtasks)
	}
	if !strings.Contains(task.Subtasks[0].String(), "StorageBackupPolicy/") {
		t.Errorf("the subtask does not name what it would create:\n%s", task.Subtasks[0])
	}
}

func TestMigrate_AnAbsorbedKindNamesTheActionItBecomes(t *testing.T) {
	plan := migratePlan(t,
		&simplyblockv1alpha1.VolumeMigration{ObjectMeta: metav1.ObjectMeta{Name: "migrate-pv-1", Namespace: "simplyblock"}},
	)

	task := taskFor(t, plan, IDAbsorbMigrations)
	if got := task.Subtasks[0].String(); !strings.Contains(got, "PersistentVolumeOps") || !strings.Contains(got, "action Migrate") {
		t.Errorf("the subtask does not say what it becomes:\n%s", got)
	}
}

func TestMigrate_RewritesAKeyAnObjectCarriesUnderTheOldPrefix(t *testing.T) {
	// Done rather than described, because it needs nothing that does not
	// exist: the inventory is data and the write is additive.
	scope := migration(t, claim("team-a", "data", map[string]string{
		"simplyblock.io/backup-policy": "nightly",
	}))

	catalog := upgrade.NewCatalog()
	catalog.Steps.MustRegister(rewriteKeys{})
	if err := upgrade.NewRunner(catalog, scope).ApplyAll(t.Context(), upgrade.StageMigrate); err != nil {
		t.Fatalf("running the rewrite: %v", err)
	}

	var written corev1.PersistentVolumeClaim
	key := types.NamespacedName{Namespace: "team-a", Name: "data"}
	if err := scope.Client.Get(t.Context(), key, &written); err != nil {
		t.Fatalf("re-reading the claim: %v", err)
	}

	if got := written.Annotations["storage.simplyblock.io/backup-policy"]; got != "nightly" {
		t.Errorf("the new key holds %q, want the value preserved verbatim", got)
	}
	// §16.3 leaves the old key for the deprecation window, so an operator
	// still reading it keeps working.
	if got := written.Annotations["simplyblock.io/backup-policy"]; got != "nightly" {
		t.Errorf("the old key was removed, and the release that still reads it is running")
	}
}

func TestMigrate_TheRewriteIsIdempotent(t *testing.T) {
	// An object carrying both spellings describes nothing, so a second run of
	// a finished migration plans no work.
	plan := migratePlan(t, claim("team-a", "data", map[string]string{
		"simplyblock.io/backup-policy":         "nightly",
		"storage.simplyblock.io/backup-policy": "nightly",
	}))

	for _, task := range plan.Tasks {
		if task.Step == IDRewriteKeys {
			t.Fatalf("the rewrite planned %v against an object already carrying both spellings",
				task.Subtasks)
		}
	}
}

func TestMigrate_DeletesARetiredKind(t *testing.T) {
	scope := migration(t,
		&simplyblockv1alpha1.BackupImport{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "simplyblock"}},
	)

	catalog := upgrade.NewCatalog()
	catalog.Steps.MustRegister(deleteBackupImports{})
	if err := upgrade.NewRunner(catalog, scope).ApplyAll(t.Context(), upgrade.StageMigrate); err != nil {
		t.Fatalf("running the deletion: %v", err)
	}

	var gone simplyblockv1alpha1.BackupImport
	key := types.NamespacedName{Namespace: "simplyblock", Name: "old"}
	if err := scope.Client.Get(t.Context(), key, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the BackupImport survived: %v", err)
	}
}

func TestMigrate_FindsALegacyVolumeHandleWithoutTheControlPlane(t *testing.T) {
	// Detecting one needs no resolver: §16.4's shape is
	// clusterID:poolID:volumeID, and the pool segment being a name rather than
	// a UUID is readable from the PersistentVolume alone.
	plan := migratePlan(t, legacyPV("pvc-legacy", legacyHandleValue))

	task := taskFor(t, plan, IDNormalizeHandles)
	if len(task.Subtasks) != 1 {
		t.Fatalf("described %d subtasks, want the one legacy volume:\n%v", len(task.Subtasks), task.Subtasks)
	}
	if got := task.Subtasks[0].String(); !strings.Contains(got, `"my-pool"`) {
		t.Errorf("the subtask does not name the pool it would resolve:\n%s", got)
	}
}

func TestMigrate_LeavesAModernHandleAlone(t *testing.T) {
	plan := migratePlan(t, legacyPV("pvc-modern", modernHandleValue))

	for _, task := range plan.Tasks {
		if task.Step == IDNormalizeHandles {
			t.Fatalf("a handle whose pool segment is already a UUID was planned for:\n%v",
				task.Subtasks)
		}
	}
}

func TestMigrate_LeavesAVolumeOfAnotherDriverAlone(t *testing.T) {
	pv := legacyPV("pvc-other", legacyHandleValue)
	pv.Spec.CSI.Driver = "ebs.csi.aws.com"

	for _, task := range migratePlan(t, pv).Tasks {
		if task.Step == IDNormalizeHandles {
			t.Fatalf("a volume this driver does not manage was planned for:\n%v", task.Subtasks)
		}
	}
}

func TestMigrate_AVolumeAlreadyCarryingTheNormalizedHandleIsFinished(t *testing.T) {
	pv := legacyPV("pvc-legacy", legacyHandleValue)
	pv.Annotations = map[string]string{"storage.simplyblock.io/volume-handle": modernHandleValue}

	scope := migration(t, pv)
	for _, step := range Migrate() {
		if step.ID() != IDNormalizeHandles {
			continue
		}
		covered, err := upgrade.Covered(t.Context(), scope, step)
		if err != nil {
			t.Fatalf("Covered: %v", err)
		}
		if len(covered.Outstanding) != 0 {
			t.Errorf("outstanding = %v on a volume already normalized", covered.Outstanding)
		}
		if len(covered.Finished) != 1 {
			t.Errorf("finished = %v, want the volume it already handled", covered.Finished)
		}
	}
}

func TestMigrate_EveryStepDescribesOrDeclines(t *testing.T) {
	// A step that panicked or errored on a subject it is not about would make
	// the plan unbuildable on a cluster holding anything unexpected.
	plan := migratePlan(t,
		claim("team-a", "data", nil),
		legacyPV("pvc-modern", modernHandleValue),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "simplyblock"}},
	)

	if len(plan.Tasks) != 0 {
		t.Fatalf("a cluster with nothing to migrate produced %d tasks:\n%v", len(plan.Tasks), plan.Tasks)
	}
}
