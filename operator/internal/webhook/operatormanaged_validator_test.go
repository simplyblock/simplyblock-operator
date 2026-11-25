// Tests of the operator-managed-fields validator: who may change a guarded
// field, what counts as a change, and that the generated table guards the
// fields the API marks.

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testOperator = "system:serviceaccount:simplyblock:simplyblock-operator"
	testHub      = "system:serviceaccount:open-cluster-management-agent:klusterlet-work-sa"
	testUser     = "alice"
	// A service account that shares the operator's namespace is not the
	// operator: the Control Center runs there too.
	testNeighbor = "system:serviceaccount:simplyblock:simplyblock-control-center"
)

var (
	clusterGVK = simplyblockv1alpha2.GroupVersion.WithKind("StorageCluster")
	cdcGVK     = simplyblockv1alpha2.GroupVersion.WithKind("ClusterDeploymentConfig")
	nodeGVK    = simplyblockv1alpha2.GroupVersion.WithKind("StorageNode")
)

func operatorManagedValidator(t *testing.T) *OperatorManagedValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &OperatorManagedValidator{
		Scheme:           scheme,
		OperatorUsername: testOperator,
		Writers:          map[string][]string{"hub": {testHub}},
	}
}

func updateRequest(t *testing.T, gvk schema.GroupVersionKind, user string, oldObj, newObj any) admission.Request {
	t.Helper()
	oldRaw, err := json.Marshal(oldObj)
	if err != nil {
		t.Fatal(err)
	}
	newRaw, err := json.Marshal(newObj)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		Kind:      metav1.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind},
		UserInfo:  authenticationv1.UserInfo{Username: user},
		Object:    runtime.RawExtension{Raw: newRaw},
		OldObject: runtime.RawExtension{Raw: oldRaw},
	}}
}

func managedStretchedCluster(role simplyblockv1alpha2.StretchedRole) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			Stretched: &simplyblockv1alpha2.StretchedSpec{Site: "a", PeerSite: "b", Role: role},
		},
	}
}

func TestAUserCannotChangeAnOperatorManagedField(t *testing.T) {
	v := operatorManagedValidator(t)
	for _, user := range []string{testUser, testNeighbor} {
		resp := v.Handle(context.Background(), updateRequest(t, clusterGVK, user,
			managedStretchedCluster(simplyblockv1alpha2.StretchedRoleLeader),
			managedStretchedCluster(simplyblockv1alpha2.StretchedRoleFollower)))
		if resp.Allowed {
			t.Errorf("%s changed spec.stretched.role on a StorageCluster, want it refused", user)
			continue
		}
		if !strings.Contains(resp.Result.Message, "spec.stretched.role") {
			t.Errorf("refusal for %s = %q, want it to name spec.stretched.role", user, resp.Result.Message)
		}
	}
}

func TestTheOperatorMayChangeAnOperatorManagedField(t *testing.T) {
	v := operatorManagedValidator(t)
	resp := v.Handle(context.Background(), updateRequest(t, clusterGVK, testOperator,
		managedStretchedCluster(simplyblockv1alpha2.StretchedRoleLeader),
		managedStretchedCluster(simplyblockv1alpha2.StretchedRoleFollower)))
	if !resp.Allowed {
		t.Fatalf("the operator's role change was refused: %s", resp.Result.Message)
	}
}

// The hub delivers a stretched site's ClusterDeploymentConfig, role swaps
// included, through its work agent; the role carries writers=hub for that.
func TestTheHubMayChangeTheStretchedRoleOfADeploymentConfig(t *testing.T) {
	v := operatorManagedValidator(t)
	cdc := func(role simplyblockv1alpha2.StretchedRole) *simplyblockv1alpha2.ClusterDeploymentConfig {
		return &simplyblockv1alpha2.ClusterDeploymentConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "site-a", Namespace: "simplyblock"},
			Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
				Cluster: &simplyblockv1alpha2.ClusterTemplate{
					Stretched: &simplyblockv1alpha2.StretchedSpec{Site: "a", PeerSite: "b", Role: role},
				},
			},
		}
	}
	oldCDC, newCDC := cdc(simplyblockv1alpha2.StretchedRoleLeader), cdc(simplyblockv1alpha2.StretchedRoleFollower)
	if resp := v.Handle(context.Background(), updateRequest(t, cdcGVK, testHub, oldCDC, newCDC)); !resp.Allowed {
		t.Fatalf("the hub's role swap was refused: %s", resp.Result.Message)
	}
	if resp := v.Handle(context.Background(), updateRequest(t, cdcGVK, testUser, oldCDC, newCDC)); resp.Allowed {
		t.Fatal("a user's role swap on a ClusterDeploymentConfig was admitted, want it refused")
	}
}

// A writer is admitted only for the fields that name it: the hub may not
// relocate a StorageNode.
func TestAWriterIsAdmittedOnlyWhereItIsNamed(t *testing.T) {
	v := operatorManagedValidator(t)
	oldNode := &simplyblockv1alpha2.StorageNode{Spec: simplyblockv1alpha2.StorageNodeSpec{WorkerNode: "w1"}}
	newNode := oldNode.DeepCopy()
	newNode.Spec.WorkerNode = "w2"
	if resp := v.Handle(context.Background(), updateRequest(t, nodeGVK, testHub, oldNode, newNode)); resp.Allowed {
		t.Fatal("the hub changed spec.workerNode, which names no writer, want it refused")
	}
}

func TestAnUpdateLeavingTheGuardedFieldsAloneIsAdmitted(t *testing.T) {
	v := operatorManagedValidator(t)
	oldNode := &simplyblockv1alpha2.StorageNode{Spec: simplyblockv1alpha2.StorageNodeSpec{WorkerNode: "w1"}}
	newNode := oldNode.DeepCopy()
	newNode.Labels = map[string]string{"team": "storage"}
	if resp := v.Handle(context.Background(), updateRequest(t, nodeGVK, testUser, oldNode, newNode)); !resp.Allowed {
		t.Fatalf("an update touching no guarded field was refused: %s", resp.Result.Message)
	}
}

// A field omitted and the same field sent as its zero value are one state to
// the Go type, and kubectl apply produces either.
func TestAnOmittedFieldAndItsZeroValueAreNoChange(t *testing.T) {
	v := operatorManagedValidator(t)
	oldNode := map[string]any{"apiVersion": "storage.simplyblock.io/v1alpha2", "kind": "StorageNode",
		"spec": map[string]any{"clusterRef": "c"}}
	newNode := map[string]any{"apiVersion": "storage.simplyblock.io/v1alpha2", "kind": "StorageNode",
		"spec": map[string]any{"clusterRef": "c", "workerNode": "", "config": map[string]any{"deviceNames": []any{}}}}
	if resp := v.Handle(context.Background(), updateRequest(t, nodeGVK, testUser, oldNode, newNode)); !resp.Allowed {
		t.Fatalf("an explicit zero value was read as a change: %s", resp.Result.Message)
	}
}

// Every guarded field a request changes is named, not only the first.
func TestTheRefusalNamesEveryChangedField(t *testing.T) {
	v := operatorManagedValidator(t)
	oldNode := &simplyblockv1alpha2.StorageNode{Spec: simplyblockv1alpha2.StorageNodeSpec{WorkerNode: "w1", SocketID: "0"}}
	newNode := oldNode.DeepCopy()
	newNode.Spec.WorkerNode, newNode.Spec.SocketID = "w2", "1"
	resp := v.Handle(context.Background(), updateRequest(t, nodeGVK, testUser, oldNode, newNode))
	if resp.Allowed {
		t.Fatal("a user's relocation was admitted, want it refused")
	}
	for _, path := range []string{"spec.workerNode", "spec.socketId"} {
		if !strings.Contains(resp.Result.Message, path) {
			t.Errorf("refusal %q does not name %s", resp.Result.Message, path)
		}
	}
}

// A create states the field for the first time: a StorageNode written by hand
// has to name its worker, so the guard is on updates alone.
func TestACreateIsAdmitted(t *testing.T) {
	v := operatorManagedValidator(t)
	req := updateRequest(t, nodeGVK, testUser, nil, &simplyblockv1alpha2.StorageNode{
		Spec: simplyblockv1alpha2.StorageNodeSpec{WorkerNode: "w1"}})
	req.Operation = admissionv1.Create
	req.OldObject = runtime.RawExtension{}
	if resp := v.Handle(context.Background(), req); !resp.Allowed {
		t.Fatalf("a create was refused: %s", resp.Result.Message)
	}
}

// Lists add no segment and maps are crossed with a star segment, as the generator writes
// them, so a change inside one element or one map value is still a change.
func TestAChangeInsideAListOrMapIsAChange(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "fixture.simplyblock.io", Version: "v1", Kind: "Widget"}
	v := operatorManagedValidator(t)
	v.Fields = map[schema.GroupVersionKind][]OperatorManagedField{gvk: {
		{Path: "spec.parts.serial", Segments: []string{"spec", "parts", "serial"}},
		{Path: "spec.byName.serial", Segments: []string{"spec", "byName", "*", "serial"}},
	}}
	widget := func(partSerial, mapSerial string) map[string]any {
		return map[string]any{"spec": map[string]any{
			"parts":  []any{map[string]any{"serial": "p0", "note": "x"}, map[string]any{"serial": partSerial}},
			"byName": map[string]any{"a": map[string]any{"serial": mapSerial, "note": "y"}},
		}}
	}
	cases := []struct {
		name     string
		newObj   map[string]any
		want     bool
		changing string
	}{
		{"unchanged", widget("p1", "m1"), true, ""},
		{"list element", widget("p2", "m1"), false, "spec.parts.serial"},
		{"map value", widget("p1", "m2"), false, "spec.byName.serial"},
	}
	for _, c := range cases {
		resp := v.Handle(context.Background(), updateRequest(t, gvk, testUser, widget("p1", "m1"), c.newObj))
		if resp.Allowed != c.want {
			t.Errorf("%s: allowed = %v, want %v (%s)", c.name, resp.Allowed, c.want, resp.Result.Message)
		}
		if !c.want && !strings.Contains(resp.Result.Message, c.changing) {
			t.Errorf("%s: refusal %q does not name %s", c.name, resp.Result.Message, c.changing)
		}
	}
}
