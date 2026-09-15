// Validation of the CEL rules compiled into the ControlPlane CRD schema, run
// against a real apiserver. It lives here rather than under internal/webhook
// because there is no webhook involved: the rules are enforced by the apiserver
// itself, and envtest is the only place in the tree that starts one.
//
// The rule that one member and only one is set is worth an apiserver test rather
// than a unit test, for two reasons. It is the whole of what makes the two modes
// siblings rather than a convention, so a reconciler asking `isManaged` has to be
// able to assume it. And it is declared on ControlPlaneSource rather than on the
// field that carries it, a placement chosen to work around a generator flake: a
// rule that silently stopped reaching the schema would look exactly like a rule
// that works.
//
// The third case below is the one that earns its keep. The design says the block
// is immutable and its members are not, and the first spelling of that here was
// +k8s:immutable on the field, which freezes the image with the block and makes
// the Upgrade action impossible to complete.

package controlplane

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Exactly one member of spec.source has to be set. Both is a control plane the
// operator would install and probe somewhere else at the same time, and neither
// is an object nothing downstream can classify.
func TestControlPlaneCELRequiresExactlyOneSource(t *testing.T) {
	apiClient := apiServer(t)

	managed := &simplyblockv1alpha2.ManagedControlPlane{Image: testImage}
	external := &simplyblockv1alpha2.ExternalControlPlane{
		Endpoint:             "https://sb-control.example.com:5000",
		CredentialsSecretRef: &corev1.LocalObjectReference{Name: "cp-token"},
	}

	for _, tc := range []struct {
		name       string
		source     simplyblockv1alpha2.ControlPlaneSource
		wantDenied bool
	}{
		{
			name:   "managed alone is what a fresh deployment writes",
			source: simplyblockv1alpha2.ControlPlaneSource{Managed: managed},
		},
		{
			name:   "external alone is a control plane that already exists",
			source: simplyblockv1alpha2.ControlPlaneSource{External: external},
		},
		{
			name:       "both would install one control plane and probe another",
			source:     simplyblockv1alpha2.ControlPlaneSource{Managed: managed, External: external},
			wantDenied: true,
		},
		{
			name:       "neither is an object nothing downstream can classify",
			source:     simplyblockv1alpha2.ControlPlaneSource{},
			wantDenied: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			namespace := freshNamespace(t, apiClient)

			cp := &simplyblockv1alpha2.ControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: SingletonName, Namespace: namespace},
				Spec:       simplyblockv1alpha2.ControlPlaneSpec{Source: tc.source},
			}

			err := apiClient.Create(context.Background(), cp)
			switch {
			case tc.wantDenied && err == nil:
				t.Fatal("the object was admitted, and exactly one of managed or external " +
					"has to be set")
			case tc.wantDenied:
				if !strings.Contains(err.Error(), "set exactly one of managed or external") {
					t.Errorf("denied with %q, want the rule's own message", err)
				}
			case err != nil:
				t.Fatalf("a legal source was denied: %v", err)
			}
		})
	}
}

// spec.source is immutable. Switching a live deployment between an installed
// control plane and an existing one is not a reconfiguration: the clusters,
// their UUIDs, and their volumes live in the FoundationDB behind the old one.
func TestControlPlaneCELRefusesToChangeTheSource(t *testing.T) {
	ctx := context.Background()
	apiClient := apiServer(t)
	namespace := freshNamespace(t, apiClient)

	cp := &simplyblockv1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonName, Namespace: namespace},
		Spec: simplyblockv1alpha2.ControlPlaneSpec{
			Source: simplyblockv1alpha2.ControlPlaneSource{
				Managed: &simplyblockv1alpha2.ManagedControlPlane{Image: testImage},
			},
		},
	}
	if err := apiClient.Create(ctx, cp); err != nil {
		t.Fatalf("create the control plane: %v", err)
	}

	cp.Spec.Source = simplyblockv1alpha2.ControlPlaneSource{
		External: &simplyblockv1alpha2.ExternalControlPlane{
			Endpoint:             "https://sb-control.example.com:5000",
			CredentialsSecretRef: &corev1.LocalObjectReference{Name: "cp-token"},
		},
	}
	err := apiClient.Update(ctx, cp)
	if err == nil {
		t.Fatal("the source was changed from managed to external, and the data behind the " +
			"old one does not move with it")
	}
	if !strings.Contains(err.Error(), "immutable") {
		t.Errorf("denied with %q, want the immutability rule", err)
	}
}

// Changing the image within a managed source is an ordinary edit, and it is what
// an Upgrade operation performs. The immutability rule covers the block rather
// than its members, so this has to stay admitted.
func TestControlPlaneCELAdmitsAnImageChangeWithinTheSameSource(t *testing.T) {
	ctx := context.Background()
	apiClient := apiServer(t)
	namespace := freshNamespace(t, apiClient)

	cp := &simplyblockv1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonName, Namespace: namespace},
		Spec: simplyblockv1alpha2.ControlPlaneSpec{
			Source: simplyblockv1alpha2.ControlPlaneSource{
				Managed: &simplyblockv1alpha2.ManagedControlPlane{Image: testImage},
			},
		},
	}
	if err := apiClient.Create(ctx, cp); err != nil {
		t.Fatalf("create the control plane: %v", err)
	}

	cp.Spec.Source.Managed.Image = "quay.io/simplyblock-io/simplyblock:26.3.0"
	if err := apiClient.Update(ctx, cp); err != nil {
		t.Fatalf("an image change was denied, and it is what an Upgrade performs: %v", err)
	}
}

// freshNamespace gives one test case a namespace of its own, so that the
// singleton name can be reused across cases against one shared apiserver.
func freshNamespace(t *testing.T, apiClient client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "cp-cel-"},
	}
	if err := apiClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create a namespace: %v", err)
	}
	return ns.Name
}

// The operation's parameters are frozen once it is admitted, so the status stays
// an audit of the request that ran. They are consumed several steps apart:
// Preflight reads the image and Applying writes it, so an edit in between
// produces an operation that checked one thing and did another.
func TestControlPlaneOpsCELFreezesTheParametersAfterAdmission(t *testing.T) {
	ctx := context.Background()
	apiClient := apiServer(t)
	namespace := freshNamespace(t, apiClient)

	ops := &simplyblockv1alpha2.ControlPlaneOps{
		ObjectMeta: metav1.ObjectMeta{Name: "an-upgrade", Namespace: namespace},
		Spec: simplyblockv1alpha2.ControlPlaneOpsSpec{
			ControlPlaneRef: SingletonName,
			Action:          simplyblockv1alpha2.ControlPlaneOpsActionUpgrade,
			Upgrade: &simplyblockv1alpha2.UpgradeSpec{
				Image: "quay.io/simplyblock-io/simplyblock:26.3.0",
			},
		},
	}
	if err := apiClient.Create(ctx, ops); err != nil {
		t.Fatalf("create the operation: %v", err)
	}

	t.Run("the image cannot be swapped", func(t *testing.T) {
		edited := ops.DeepCopy()
		edited.Spec.Upgrade.Image = "quay.io/simplyblock-io/simplyblock:26.9.9"
		if err := apiClient.Update(ctx, edited); err == nil {
			t.Error("the image was changed after admission, so Preflight checked one " +
				"version and Applying would write another")
		}
	})

	t.Run("the block cannot be cleared", func(t *testing.T) {
		edited := ops.DeepCopy()
		edited.Spec.Upgrade = nil
		if err := apiClient.Update(ctx, edited); err == nil {
			t.Error("spec.upgrade was cleared after admission, which is what Applying " +
				"would then dereference")
		}
	})

	t.Run("abort stays settable", func(t *testing.T) {
		edited := ops.DeepCopy()
		edited.Spec.Abort = true
		if err := apiClient.Update(ctx, edited); err != nil {
			t.Errorf("spec.abort could not be set, and it is the one field meant to be "+
				"changed after the operation started: %v", err)
		}
	})
}

// A restart's scope is frozen for the same reason: the drain is decided from the
// component list, so widening it after Draining skips a drain the wider list
// would have required.
func TestControlPlaneOpsCELFreezesTheRestartScope(t *testing.T) {
	ctx := context.Background()
	apiClient := apiServer(t)
	namespace := freshNamespace(t, apiClient)

	ops := &simplyblockv1alpha2.ControlPlaneOps{
		ObjectMeta: metav1.ObjectMeta{Name: "a-restart", Namespace: namespace},
		Spec: simplyblockv1alpha2.ControlPlaneOpsSpec{
			ControlPlaneRef: SingletonName,
			Action:          simplyblockv1alpha2.ControlPlaneOpsActionRestart,
			Restart: &simplyblockv1alpha2.RestartSpec{
				Components: []string{ComponentTasks},
			},
		},
	}
	if err := apiClient.Create(ctx, ops); err != nil {
		t.Fatalf("create the operation: %v", err)
	}

	ops.Spec.Restart.Components = []string{ComponentTasks, ComponentWebAPI}
	if err := apiClient.Update(ctx, ops); err == nil {
		t.Error("an essential component was added to the scope after admission, which " +
			"would recycle the management API without the drain that scope requires")
	}
}
