package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func newFakeVolumeMigrations(objects ...runtime.Object) (*volumeMigrations, *dynamicfake.FakeDynamicClient) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{volumeMigrationGVR: "VolumeMigrationList"}, objects...)
	return &volumeMigrations{client: client, namespace: "simplyblock"}, client
}

func TestEnsureCreatesTheRequestOnceAndReportsItsPhase(t *testing.T) {
	m, client := newFakeVolumeMigrations()
	phase, err := m.Ensure(context.Background(), "cg-join-v1", "pv-1", "node-pin")
	if err != nil || phase != "" {
		t.Fatalf("first Ensure: phase %q err %v", phase, err)
	}
	got, err := client.Resource(volumeMigrationGVR).Namespace("simplyblock").
		Get(context.Background(), "cg-join-v1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the VolumeMigration was not created: %v", err)
	}
	pv, _, _ := unstructured.NestedString(got.Object, "spec", "pvName")
	target, _, _ := unstructured.NestedString(got.Object, "spec", "targetNodeUUID")
	if pv != "pv-1" || target != "node-pin" {
		t.Fatalf("spec = %s -> %s, want pv-1 -> node-pin", pv, target)
	}
	if got.GetLabels()[preJoinPurposeLabel] != "consistency-group-join" {
		t.Fatalf("missing the purpose label: %v", got.GetLabels())
	}

	if err := unstructured.SetNestedField(got.Object, "Completed", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(volumeMigrationGVR).Namespace("simplyblock").
		Update(context.Background(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	phase, err = m.Ensure(context.Background(), "cg-join-v1", "pv-1", "other-node")
	if err != nil || phase != "Completed" {
		t.Fatalf("second Ensure: phase %q err %v", phase, err)
	}
	again, _ := client.Resource(volumeMigrationGVR).Namespace("simplyblock").
		Get(context.Background(), "cg-join-v1", metav1.GetOptions{})
	if target, _, _ := unstructured.NestedString(again.Object, "spec", "targetNodeUUID"); target != "node-pin" {
		t.Fatalf("an existing request must never be rewritten, target is now %s", target)
	}
}

func TestWatcherOptionsDefaults(t *testing.T) {
	t.Setenv("SPDKCSI_CG_PREJOIN_MIGRATION", "")
	t.Setenv("SPDKCSI_CG_COLOCATE", "")
	t.Setenv("SPDKCSI_CG_CLIENT_SWAP_READY", "")
	preJoin, colocate, swap := watcherOptions()
	if !preJoin || colocate || swap {
		t.Fatalf("defaults = %v %v %v, want true false false", preJoin, colocate, swap)
	}
	t.Setenv("SPDKCSI_CG_PREJOIN_MIGRATION", "off")
	t.Setenv("SPDKCSI_CG_COLOCATE", "true")
	preJoin, colocate, _ = watcherOptions()
	if preJoin || !colocate {
		t.Fatalf("overrides = %v %v, want false true", preJoin, colocate)
	}
}
