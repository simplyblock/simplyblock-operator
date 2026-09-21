/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The scenario matrix this file implements is
// docs/tests/test-plan-ramen-integration.md §1's U-01…U-03, verifying
// design-ramen-integration.md §4.3's fan-in aggregation.

const vgrCluster = "55555555-5555-5555-5555-555555555555"

// vgrUUID maps a short fixture id to a deterministic canonical UUID, the same
// way replication_test.go's sibling in csi-driver and the webhook package's
// vgsUUID both do, since atlas/lvol.ParseHandle accepts only canonical UUIDs.
func vgrUUID(id string) string {
	sum := sha256.Sum256([]byte(id))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// vgrBackend serves the consistency-group resolve + membership reads.
type vgrBackend struct {
	members []string // short fixture ids, converted through vgrUUID
}

func (b vgrBackend) start(t *testing.T) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/clusters/"+vgrCluster+"/consistency-groups/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/members") {
			rows := make([]map[string]any, 0, len(b.members))
			for _, m := range b.members {
				rows = append(rows, map[string]any{"lvol_id": vgrUUID(m)})
			}
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "grp", "name": "grp1", "member_count": len(b.members)},
		})
	})
	srv := newAPIServer(t, mux.ServeHTTP)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
}

// vgrMember describes one group member: a bound PVC plus the per-volume
// VolumeReplication a prior fan-out already created for it, pre-seeded with
// the status this test wants fan-in to read.
type vgrMember struct {
	pvcName   string
	volumeID  string // short fixture id, converted through vgrUUID
	completed bool
	degraded  bool
	lastSync  time.Time
}

func newVGRReconciler(t *testing.T, objects ...client.Object) (*VolumeGroupReplicationReconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t, corev1.AddToScheme)
	statusSubresource := &unstructured.Unstructured{}
	statusSubresource.SetGroupVersionKind(volumeGroupReplicationGVK)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(statusSubresource).
		WithObjects(objects...).
		Build()
	return &VolumeGroupReplicationReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(32),
	}, cl
}

func vgrClass(name string) *unstructured.Unstructured {
	class := &unstructured.Unstructured{}
	class.SetGroupVersionKind(volumeGroupReplicationClassGVK)
	class.SetName(name)
	_ = unstructured.SetNestedField(class.Object, "csi.simplyblock.io", "spec", "provisioner")
	return class
}

// vgrGroup builds the VolumeGroupReplication under test, selecting every
// member by the shared "app: grp1" label.
func vgrGroup(name, className, vrClassName, replicationState string) *unstructured.Unstructured {
	vgr := &unstructured.Unstructured{}
	vgr.SetGroupVersionKind(volumeGroupReplicationGVK)
	vgr.SetName(name)
	vgr.SetNamespace("default")
	vgr.SetUID(types.UID("vgr-uid-" + name))
	spec := map[string]interface{}{
		"external":                        true,
		"autoResync":                      false,
		"replicationState":                replicationState,
		"volumeGroupReplicationClassName": className,
		"volumeReplicationClassName":      vrClassName,
		"source": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"app": "grp1"},
			},
		},
	}
	_ = unstructured.SetNestedMap(vgr.Object, spec, "spec")
	return vgr
}

// vgrPVCAndPV builds a bound PVC/PV pair: labeled for both the group
// selector and the consistency-group membership check, backed by a
// simplyblock CSI volume handle.
func vgrPVCAndPV(pvcName, volumeID string) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	pvName := "pv-" + pvcName
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: "default",
			Labels: map[string]string{
				"app":                 "grp1",
				consistencyGroupLabel: "grp1",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{
				Driver:       "csi.simplyblock.io",
				VolumeHandle: vgrCluster + ":pool:" + vgrUUID(volumeID),
			},
		}},
	}
	return pvc, pv
}

// vgrMemberVR builds the per-volume VolumeReplication a prior fan-out
// already created for one group member, with the status this test wants
// fan-in to aggregate.
func vgrMemberVR(name, groupName, pvcName string, m vgrMember) *unstructured.Unstructured {
	vr := &unstructured.Unstructured{}
	vr.SetGroupVersionKind(volumeReplicationGVK)
	vr.SetName(name)
	vr.SetNamespace("default")
	vr.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: volumeGroupReplicationGVK.GroupVersion().String(),
		Kind:       volumeGroupReplicationGVK.Kind,
		Name:       groupName,
		UID:        types.UID("vgr-uid-" + groupName),
	}})
	_ = unstructured.SetNestedField(vr.Object, pvcName, "spec", "dataSource", "name")
	_ = unstructured.SetNestedField(vr.Object, "PersistentVolumeClaim", "spec", "dataSource", "kind")
	_ = unstructured.SetNestedField(vr.Object, "primary", "spec", "replicationState")

	cond := func(condType string, status bool) map[string]interface{} {
		s := "False"
		if status {
			s = "True"
		}
		return map[string]interface{}{"type": condType, "status": s, "reason": "Test", "message": ""}
	}
	conditions := []interface{}{
		cond("Completed", m.completed),
		cond("Degraded", m.degraded),
		cond("Resyncing", false),
	}
	_ = unstructured.SetNestedSlice(vr.Object, conditions, "status", "conditions")
	_ = unstructured.SetNestedField(vr.Object, m.lastSync.UTC().Format(time.RFC3339), "status", "lastSyncTime")
	return vr
}

// runVGRReconcile builds the group, its class, the member PVC/PV pairs, and
// their pre-seeded VolumeReplications, reconciles once, and returns the
// group's own status.conditions and status.lastSyncTime for assertion.
func runVGRReconcile(t *testing.T, members []vgrMember) (conditions map[string]string, lastSyncTime string) {
	t.Helper()
	vgrBackend{members: memberIDs(members)}.start(t)

	group := vgrGroup("vgr1", "sb-group-class", "sb-vr-class", "primary")
	objs := []client.Object{group, vgrClass("sb-group-class")}
	var memberIDsOnly []string
	for _, m := range members {
		pvc, pv := vgrPVCAndPV(m.pvcName, m.volumeID)
		objs = append(objs, pvc, pv)
		memberIDsOnly = append(memberIDsOnly, m.volumeID)
	}
	r, cl := newVGRReconciler(t, objs...)
	for _, m := range members {
		vr := vgrMemberVR("vgr1-"+m.pvcName, "vgr1", m.pvcName, m)
		if err := cl.Create(context.Background(), vr); err != nil {
			t.Fatalf("create member VolumeReplication: %v", err)
		}
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "vgr1"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(volumeGroupReplicationGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "vgr1"}, got); err != nil {
		t.Fatalf("get VolumeGroupReplication: %v", err)
	}

	conditions = map[string]string{}
	rawConds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	for _, c := range rawConds {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		conditions[cm["type"].(string)] = cm["status"].(string)
	}
	lastSyncTime, _, _ = unstructured.NestedString(got.Object, "status", "lastSyncTime")
	return conditions, lastSyncTime
}

func memberIDs(members []vgrMember) []string {
	var ids []string
	for _, m := range members {
		ids = append(ids, m.volumeID)
	}
	return ids
}

// U-01: every member Completed/not-Degraded yields a group reporting the same.
func TestVolumeGroupReplication_AllMembersHealthyYieldsGroupHealthy(t *testing.T) {
	conditions, _ := runVGRReconcile(t, []vgrMember{
		{pvcName: "pvc-a", volumeID: "v1", completed: true, degraded: false, lastSync: time.Now()},
		{pvcName: "pvc-b", volumeID: "v2", completed: true, degraded: false, lastSync: time.Now()},
	})
	if conditions["Completed"] != "True" {
		t.Errorf("Completed = %q, want True", conditions["Completed"])
	}
	if conditions["Degraded"] != "False" {
		t.Errorf("Degraded = %q, want False", conditions["Degraded"])
	}
}

// U-02: one member Degraded yields a group reporting Degraded.
func TestVolumeGroupReplication_OneMemberDegradedYieldsGroupDegraded(t *testing.T) {
	conditions, _ := runVGRReconcile(t, []vgrMember{
		{pvcName: "pvc-a", volumeID: "v1", completed: true, degraded: false, lastSync: time.Now()},
		{pvcName: "pvc-b", volumeID: "v2", completed: true, degraded: true, lastSync: time.Now()},
	})
	if conditions["Degraded"] != "True" {
		t.Errorf("Degraded = %q, want True", conditions["Degraded"])
	}
}

// U-03: members with differing lastSyncTime yield the oldest, not the newest.
func TestVolumeGroupReplication_LastSyncTimeIsTheOldestMember(t *testing.T) {
	older := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	newer := time.Now().Truncate(time.Second)
	_, lastSyncTime := runVGRReconcile(t, []vgrMember{
		{pvcName: "pvc-a", volumeID: "v1", completed: true, degraded: false, lastSync: older},
		{pvcName: "pvc-b", volumeID: "v2", completed: true, degraded: false, lastSync: newer},
	})
	want := older.UTC().Format(time.RFC3339)
	if lastSyncTime != want {
		t.Errorf("status.lastSyncTime = %q, want the oldest member's %q", lastSyncTime, want)
	}
}
