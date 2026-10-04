package webhook

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/volume"
)

const (
	pvopsVolume  = "pvc-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	pvopsCluster = "11111111-1111-1111-1111-111111111111"
	pvopsPool    = "22222222-2222-2222-2222-222222222222"
	pvopsLvol    = "33333333-3333-3333-3333-333333333333"
	pvopsNode    = "worker-5"
	pvopsNS      = "simplyblock"

	// pvopsOtherNS is where the second cluster lives, which is what makes a
	// target in the wrong cluster writable at all.
	pvopsOtherNS = "other"
)

// pvopsValidator builds the validator over a world the test describes.
func pvopsValidator(t *testing.T, objs ...client.Object) *PersistentVolumeOpsValidator {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build the scheme: %v", err)
		}
	}
	return &PersistentVolumeOpsValidator{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
	}
}

// pvopsOperation is a well-formed migration, which every case below breaks in
// exactly one way.
func pvopsOperation() *simplyblockv1alpha2.PersistentVolumeOps {
	return &simplyblockv1alpha2.PersistentVolumeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "move-1"},
		Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
			PersistentVolumeName: pvopsVolume,
			Action:               simplyblockv1alpha2.PersistentVolumeOpsActionMigrate,
			Migrate: &simplyblockv1alpha2.MigrateVolumeSpec{
				TargetNodeRef: simplyblockv1alpha2.StorageNodeReference{
					Namespace: pvopsNS,
					Name:      pvopsNode,
				},
			},
		},
	}
}

// simplyblockVolume is a PersistentVolume this operator can act on.
func simplyblockVolume() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvopsVolume},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "csi.simplyblock.io",
					VolumeHandle: pvopsCluster + ":" + pvopsPool + ":" + pvopsLvol,
				},
			},
		},
	}
}

func pvopsClusterObject() *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: pvopsNS},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: pvopsCluster},
	}
}

func pvopsNodeObject() *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: pvopsNode, Namespace: pvopsNS},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: "production"},
	}
}

func pvopsCreateRequest(t *testing.T, ops *simplyblockv1alpha2.PersistentVolumeOps) admission.Request {
	t.Helper()
	raw, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("encoding the operation: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Name:      ops.Name,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func pvopsDeleteRequest(t *testing.T, ops *simplyblockv1alpha2.PersistentVolumeOps) admission.Request {
	t.Helper()
	raw, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("encoding the operation: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		Name:      ops.Name,
		OldObject: runtime.RawExtension{Raw: raw},
	}}
}

// TestPersistentVolumeOpsAdmitsAWellFormedMigration is the positive half every
// refusal below is measured against: the same request, with nothing wrong.
func TestPersistentVolumeOpsAdmitsAWellFormedMigration(t *testing.T) {
	v := pvopsValidator(t, simplyblockVolume(), pvopsClusterObject(), pvopsNodeObject())

	got := v.Handle(context.Background(), pvopsCreateRequest(t, pvopsOperation()))
	if !got.Allowed {
		t.Fatalf("a well-formed migration was refused: %s", got.Result.Message)
	}
}

// TestPersistentVolumeOpsRefusesWhatNoReconcileCouldFix. Each row is a
// condition fixed for the object's whole life, which is the line this webhook
// draws: a fact about now is a phase and an event, and a fact that can never
// change is a rejection.
func TestPersistentVolumeOpsRefusesWhatNoReconcileCouldFix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		world func() []client.Object
		ops   func() *simplyblockv1alpha2.PersistentVolumeOps
		says  string
	}{
		{
			name: "a volume that does not exist",
			world: func() []client.Object {
				return []client.Object{pvopsClusterObject(), pvopsNodeObject()}
			},
			ops:  pvopsOperation,
			says: "no PersistentVolume",
		},
		{
			name: "a volume with no CSI source at all",
			world: func() []client.Object {
				pv := simplyblockVolume()
				pv.Spec.CSI = nil
				pv.Spec.HostPath = &corev1.HostPathVolumeSource{Path: "/mnt/data"}
				return []client.Object{pv, pvopsClusterObject(), pvopsNodeObject()}
			},
			ops:  pvopsOperation,
			says: "no CSI",
		},
		{
			// The row that earns the webhook. Every other rejection here is a
			// malformed request; this one is a well-formed request against the
			// wrong object, and it is the mistake somebody writing one by hand
			// is likeliest to make, because `kubectl get pv` lists every volume
			// in the cluster and says nothing about which of them this operator
			// can move.
			name: "a volume another driver provisioned",
			world: func() []client.Object {
				pv := simplyblockVolume()
				pv.Spec.CSI.Driver = "ebs.csi.aws.com"
				return []client.Object{pv, pvopsClusterObject(), pvopsNodeObject()}
			},
			ops:  pvopsOperation,
			says: "ebs.csi.aws.com",
		},
		{
			name: "a handle that is not three parts",
			world: func() []client.Object {
				pv := simplyblockVolume()
				pv.Spec.CSI.VolumeHandle = pvopsLvol
				return []client.Object{pv, pvopsClusterObject(), pvopsNodeObject()}
			},
			ops:  pvopsOperation,
			says: "volume handle",
		},
		{
			name: "a target node that does not exist",
			world: func() []client.Object {
				return []client.Object{simplyblockVolume(), pvopsClusterObject()}
			},
			ops:  pvopsOperation,
			says: "no StorageNode",
		},
		{
			// The namespace is carried explicitly so a hand-written spec can be
			// read without performing a join, which makes this mistake
			// writable: two clusters in two namespaces may each hold a node
			// called worker-5.
			name: "a target node in another cluster than the volume",
			world: func() []client.Object {
				elsewhere := pvopsNodeObject()
				elsewhere.Namespace = pvopsOtherNS
				elsewhere.Spec.ClusterRef = "staging"
				other := pvopsClusterObject()
				other.Name, other.Namespace = "staging", pvopsOtherNS
				other.Status.UUID = "99999999-9999-9999-9999-999999999999"
				return []client.Object{simplyblockVolume(), pvopsClusterObject(), other, elsewhere}
			},
			ops: func() *simplyblockv1alpha2.PersistentVolumeOps {
				ops := pvopsOperation()
				ops.Spec.Migrate.TargetNodeRef.Namespace = pvopsOtherNS
				return ops
			},
			says: "cluster",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := pvopsValidator(t, tc.world()...)

			got := v.Handle(context.Background(), pvopsCreateRequest(t, tc.ops()))
			if got.Allowed {
				t.Fatal("the request was admitted")
			}
			if !strings.Contains(got.Result.Message, tc.says) {
				t.Errorf("refused for the wrong reason: %s", got.Result.Message)
			}
		})
	}
}

// TestPersistentVolumeOpsAdmitsWhatIsMerelyTrueNow. A target node that is
// offline and a cluster that has not been created in the backend yet are both
// conditions the next reconcile may find changed, and an admission decision is
// made once and never revisited.
func TestPersistentVolumeOpsAdmitsWhatIsMerelyTrueNow(t *testing.T) {
	t.Run("a cluster with no UUID yet", func(t *testing.T) {
		cluster := pvopsClusterObject()
		cluster.Status.UUID = ""
		v := pvopsValidator(t, simplyblockVolume(), cluster, pvopsNodeObject())

		got := v.Handle(context.Background(), pvopsCreateRequest(t, pvopsOperation()))
		if !got.Allowed {
			t.Errorf("refused a cluster that has simply not been created yet: %s", got.Result.Message)
		}
	})

	t.Run("a target node that is offline", func(t *testing.T) {
		node := pvopsNodeObject()
		node.Status.Phase = simplyblockv1alpha2.StorageNodePhaseOffline
		v := pvopsValidator(t, simplyblockVolume(), pvopsClusterObject(), node)

		got := v.Handle(context.Background(), pvopsCreateRequest(t, pvopsOperation()))
		if !got.Allowed {
			t.Errorf("refused a node that is merely offline right now: %s", got.Result.Message)
		}
	})
}

// TestPersistentVolumeOpsRefusesADeleteAfterTheCutover. Verifying holds the
// validation Jobs and the paths they connected, and the production defect this
// step exists for is exactly those paths outliving the object that recorded
// them.
func TestPersistentVolumeOpsRefusesADeleteAfterTheCutover(t *testing.T) {
	v := pvopsValidator(t, simplyblockVolume(), pvopsClusterObject(), pvopsNodeObject())

	ops := pvopsOperation()
	ops.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning
	ops.Status.Step = statemachine.KubeSnapshot{
		State: string(simplyblockv1alpha2.PersistentVolumeOpsStepVerifying),
	}

	got := v.Handle(context.Background(), pvopsDeleteRequest(t, ops))
	if got.Allowed {
		t.Fatal("a delete at Verifying was admitted")
	}
	if !strings.Contains(got.Result.Message, "Verifying") {
		t.Errorf("the refusal does not name the step: %s", got.Result.Message)
	}
	if !strings.Contains(got.Result.Message, "abort") {
		t.Errorf("the refusal does not say what to do instead: %s", got.Result.Message)
	}
}

// TestPersistentVolumeOpsAdmitsADeleteTheAbortCouldExpress. A delete arriving
// where the graph still declares an abort edge is admitted and unwound by the
// finalizer, because the two channels have to agree.
func TestPersistentVolumeOpsAdmitsADeleteTheAbortCouldExpress(t *testing.T) {
	v := pvopsValidator(t, simplyblockVolume(), pvopsClusterObject(), pvopsNodeObject())

	for _, at := range []simplyblockv1alpha2.PersistentVolumeOpsStep{
		simplyblockv1alpha2.PersistentVolumeOpsStepValidating,
		simplyblockv1alpha2.PersistentVolumeOpsStepMigrating,
	} {
		t.Run(string(at), func(t *testing.T) {
			ops := pvopsOperation()
			ops.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning
			ops.Status.Step = statemachine.KubeSnapshot{State: string(at)}

			if got := v.Handle(context.Background(), pvopsDeleteRequest(t, ops)); !got.Allowed {
				t.Errorf("a delete at %s was refused: %s", at, got.Result.Message)
			}
		})
	}
}

// TestPersistentVolumeOpsAdmitsADeleteOfAFinishedOperation. A terminal
// operation is a record of work that has finished, and withdrawing it stops
// nothing.
func TestPersistentVolumeOpsAdmitsADeleteOfAFinishedOperation(t *testing.T) {
	v := pvopsValidator(t, simplyblockVolume(), pvopsClusterObject(), pvopsNodeObject())

	for _, phase := range []simplyblockv1alpha2.PersistentVolumeOpsPhase{
		simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded,
		simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed,
		simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted,
	} {
		t.Run(string(phase), func(t *testing.T) {
			ops := pvopsOperation()
			ops.Status.Phase = phase
			// Terminal at the step a running operation would be refused from,
			// which is the case the phase check has to come before the step's.
			ops.Status.Step = statemachine.KubeSnapshot{
				State: string(simplyblockv1alpha2.PersistentVolumeOpsStepVerifying),
			}

			if got := v.Handle(context.Background(), pvopsDeleteRequest(t, ops)); !got.Allowed {
				t.Errorf("a delete of a %s operation was refused: %s", phase, got.Result.Message)
			}
		})
	}
}

// TestPersistentVolumeOpsDeleteGuardAgreesWithTheGraph. The rule is the
// group's rather than this kind's: a deletion may never express a stop that
// spec.abort could not, so both channels read one graph. This holds the
// webhook's table and the graph equal in both directions — an entry here for a
// step the graph can abort from refuses a delete the abort channel would have
// honored, and a missing entry admits the withdrawal of a record nothing else
// accounts for.
func TestPersistentVolumeOpsDeleteGuardAgreesWithTheGraph(t *testing.T) {
	refused := volume.UnabortableSteps()

	for step := range undeletableVolumeSteps {
		if !slices.Contains(refused, step) {
			t.Errorf("the guard refuses a delete at %s, which the graph can abort from", step)
		}
	}
	for _, step := range refused {
		if _, guarded := undeletableVolumeSteps[step]; !guarded {
			t.Errorf("the graph declares no abort from %s, and the guard admits a delete there", step)
		}
	}
}

// TestPersistentVolumeOpsIgnoresEverythingButCreateAndDelete. An update is the
// abort channel, which the controller reads rather than admission.
func TestPersistentVolumeOpsIgnoresEverythingButCreateAndDelete(t *testing.T) {
	v := pvopsValidator(t)

	req := pvopsDeleteRequest(t, pvopsOperation())
	req.Operation = admissionv1.Update

	if got := v.Handle(context.Background(), req); !got.Allowed {
		t.Errorf("an update was refused: %s", got.Result.Message)
	}
}
