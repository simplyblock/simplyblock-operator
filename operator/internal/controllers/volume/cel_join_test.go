// Validation of the CEL rules and schema bounds the JoinConsistencyGroup action
// adds to the PersistentVolumeOps CRD, run against a real apiserver.
//
// The objects are unstructured so that each case states the wire form a user
// writes, and so that a schema missing the action fails the case at the
// apiserver rather than at compile time (design-cg-live-subsystem-flip.md
// Appendix A, test plan U-99 to U-103).

package volume

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var persistentVolumeOpsGVK = schema.GroupVersionKind{
	Group: "storage.simplyblock.io", Version: "v1alpha2", Kind: "PersistentVolumeOps",
}

// joinOperation is a well-formed JoinConsistencyGroup, which every case below
// starts from and breaks in exactly one way.
func joinOperation() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"generateName": "cel-join-"},
		"spec": map[string]any{
			"persistentVolumeName": "pvc-0001",
			"action":               "JoinConsistencyGroup",
			"joinConsistencyGroup": map[string]any{"groupName": "orders-db"},
		},
	}}
	u.SetGroupVersionKind(persistentVolumeOpsGVK)
	return u
}

func setJoin(u *unstructured.Unstructured, field string, value any) {
	if err := unstructured.SetNestedField(u.Object, value, "spec", "joinConsistencyGroup", field); err != nil {
		panic(err)
	}
}

func createExpecting(t *testing.T, apiClient client.Client, u *unstructured.Unstructured, want string) {
	t.Helper()
	ctx := context.Background()
	err := apiClient.Create(ctx, u)
	if want == "" {
		if err != nil {
			t.Fatalf("a well-formed operation was rejected: %v", err)
		}
		t.Cleanup(func() { _ = apiClient.Delete(ctx, u) })
		return
	}
	if err == nil {
		_ = apiClient.Delete(ctx, u)
		t.Fatalf("the apiserver accepted it, want a rejection mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("rejected for the wrong reason: %v", err)
	}
}

// U-99: JoinConsistencyGroup requires spec.joinConsistencyGroup, and no other
// action accepts it.
func TestJoinConsistencyGroupCELPairsTheActionWithItsBlock(t *testing.T) {
	apiClient := apiServer(t)

	t.Run("JoinConsistencyGroup with its block is accepted", func(t *testing.T) {
		createExpecting(t, apiClient, joinOperation(), "")
	})
	t.Run("JoinConsistencyGroup without its block is rejected", func(t *testing.T) {
		u := joinOperation()
		unstructured.RemoveNestedField(u.Object, "spec", "joinConsistencyGroup")
		createExpecting(t, apiClient, u, "joinConsistencyGroup is required for action JoinConsistencyGroup")
	})
	t.Run("Migrate carrying a joinConsistencyGroup block is rejected", func(t *testing.T) {
		u := joinOperation()
		_ = unstructured.SetNestedField(u.Object, "Migrate", "spec", "action")
		_ = unstructured.SetNestedMap(u.Object, map[string]any{
			"targetNodeRef": map[string]any{"namespace": "simplyblock", "name": "worker-3"},
		}, "spec", "migrate")
		createExpecting(t, apiClient, u, "must be absent otherwise")
	})
}

// U-100: groupName is a label value, so it is bounded like one.
func TestJoinConsistencyGroupCELBoundsTheGroupName(t *testing.T) {
	apiClient := apiServer(t)
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty is rejected", value: "", want: "groupName"},
		{name: "63 characters is accepted", value: strings.Repeat("g", 63)},
		{name: "64 characters is rejected", value: strings.Repeat("g", 64), want: "groupName"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := joinOperation()
			setJoin(u, "groupName", tc.value)
			createExpecting(t, apiClient, u, tc.want)
		})
	}
}

// U-101: the suspend window is bounded, and defaults to ten seconds.
func TestJoinConsistencyGroupCELBoundsTheSuspendTimeout(t *testing.T) {
	apiClient := apiServer(t)
	for _, tc := range []struct {
		name  string
		value int64
		want  string
	}{
		{name: "0 is rejected", value: 0, want: "suspendTimeoutSeconds"},
		{name: "1 is accepted", value: 1},
		{name: "30 is accepted", value: 30},
		{name: "31 is rejected", value: 31, want: "suspendTimeoutSeconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := joinOperation()
			setJoin(u, "suspendTimeoutSeconds", tc.value)
			createExpecting(t, apiClient, u, tc.want)
		})
	}

	t.Run("unset defaults to 10", func(t *testing.T) {
		u := joinOperation()
		createExpecting(t, apiClient, u, "")
		got, found, err := unstructured.NestedInt64(u.Object, "spec", "joinConsistencyGroup", "suspendTimeoutSeconds")
		if err != nil || !found || got != 10 {
			t.Fatalf("suspendTimeoutSeconds = %d (found %v, err %v), want the default 10", got, found, err)
		}
	})
}

// U-102: the request is frozen once written, the group and the window with it.
func TestJoinConsistencyGroupCELFreezesTheRequest(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()
	for _, tc := range []struct {
		field string
		value any
	}{
		{field: "groupName", value: "billing-db"},
		{field: "suspendTimeoutSeconds", value: int64(20)},
	} {
		t.Run(tc.field, func(t *testing.T) {
			u := joinOperation()
			setJoin(u, "suspendTimeoutSeconds", int64(10))
			createExpecting(t, apiClient, u, "")

			setJoin(u, tc.field, tc.value)
			err := apiClient.Update(ctx, u)
			if err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("Update = %v, want %s refused as immutable", err, tc.field)
			}
		})
	}
}

// U-103: the action's steps are declared on the status, and a step no action
// declares is still refused.
func TestJoinConsistencyGroupCELAcceptsItsSteps(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()
	u := joinOperation()
	createExpecting(t, apiClient, u, "")

	for _, tc := range []struct {
		step string
		want string
	}{
		{step: "Planning"},
		{step: "Joining"},
		{step: "SelectingSubsystem"},
		{step: "PreparingTarget"},
		{step: "Suspending"},
		{step: "MovingNamespaces"},
		{step: "Resuming"},
		{step: "ReleasingPaths"},
		{step: "Draining", want: "unknown step"},
	} {
		t.Run(tc.step, func(t *testing.T) {
			fresh := &unstructured.Unstructured{}
			fresh.SetGroupVersionKind(persistentVolumeOpsGVK)
			if err := apiClient.Get(ctx, client.ObjectKeyFromObject(u), fresh); err != nil {
				t.Fatalf("reading the operation back: %v", err)
			}
			base := fresh.DeepCopy()
			_ = unstructured.SetNestedField(fresh.Object, tc.step, "status", "step", "state")
			err := apiClient.Status().Patch(ctx, fresh, client.MergeFrom(base))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("the apiserver rejected declared step %q: %v", tc.step, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Patch = %v, want step %q refused", err, tc.step)
			}
		})
	}
}
