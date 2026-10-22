// Admission tests for ReplicationOps: the migration-only fields (scope=cluster,
// spec.policyRef, spec.dryRun) and the references scope=cluster resolves.

package webhook

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const replopsNS = "default"

func replopsValidator(t *testing.T, objs ...client.Object) *ReplicationOpsValidator {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		simplyblockv1alpha1.AddToScheme,
		simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build the scheme: %v", err)
		}
	}
	return &ReplicationOpsValidator{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()}
}

func replopsAdmit(t *testing.T, v *ReplicationOpsValidator, spec simplyblockv1alpha1.ReplicationOpsSpec) admission.Response {
	t.Helper()
	ops := &simplyblockv1alpha1.ReplicationOps{
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.simplyblock.io/v1alpha1", Kind: "ReplicationOps"},
		ObjectMeta: metav1.ObjectMeta{Name: "ops1", Namespace: replopsNS},
		Spec:       spec,
	}
	raw, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create}}
	req.Object.Raw = raw
	return v.Handle(context.Background(), req)
}

func replopsPolicy(name, mode string) *simplyblockv1alpha1.ReplicationPolicy {
	return &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: replopsNS},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1", Mode: mode},
	}
}

const replopsClusterUUID = "11111111-2222-4333-8444-555555555555"

func TestReplicationOpsValidator_ClusterScopeMigration_Allowed(t *testing.T) {
	v := replopsValidator(t, replopsPolicy("mig", "migration"))
	resp := replopsAdmit(t, v, simplyblockv1alpha1.ReplicationOpsSpec{
		Action: "migration", Scope: utils.ReplicationOpsScopeCluster, Ref: replopsClusterUUID, PolicyRef: "mig",
	})
	if !resp.Allowed {
		t.Fatalf("denied a valid cluster migration: %s", resp.Result.Message)
	}
}

func TestReplicationOpsValidator_Denials(t *testing.T) {
	cases := []struct {
		name string
		spec simplyblockv1alpha1.ReplicationOpsSpec
	}{
		{"cluster scope with failover", simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failover", Scope: utils.ReplicationOpsScopeCluster, Ref: replopsClusterUUID, PolicyRef: "mig"}},
		{"cluster scope without policyRef", simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "migration", Scope: utils.ReplicationOpsScopeCluster, Ref: replopsClusterUUID}},
		{"policyRef outside cluster scope", simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "migration", Scope: utils.ReplicationOpsScopePolicy, Ref: "mig", PolicyRef: "mig"}},
		{"dryRun with failover", simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failover", Scope: utils.ReplicationOpsScopePolicy, Ref: "mig", DryRun: true}},
		{"policyRef in failover mode", simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "migration", Scope: utils.ReplicationOpsScopeCluster, Ref: replopsClusterUUID, PolicyRef: "dr"}},
		{"policyRef that does not exist", simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "migration", Scope: utils.ReplicationOpsScopeCluster, Ref: replopsClusterUUID, PolicyRef: "nope"}},
		{"unknown storage cluster name", simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "migration", Scope: utils.ReplicationOpsScopeCluster, Ref: "no-such-cluster", PolicyRef: "mig"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := replopsValidator(t, replopsPolicy("mig", "migration"), replopsPolicy("dr", "failover"))
			if resp := replopsAdmit(t, v, tc.spec); resp.Allowed {
				t.Fatalf("allowed %+v, want a denial", tc.spec)
			}
		})
	}
}

func TestReplicationOpsValidator_MigrationDryRunOnPolicy_Allowed(t *testing.T) {
	v := replopsValidator(t, replopsPolicy("mig", "migration"))
	resp := replopsAdmit(t, v, simplyblockv1alpha1.ReplicationOpsSpec{
		Action: "migration", Scope: utils.ReplicationOpsScopePolicy, Ref: "mig", DryRun: true,
	})
	if !resp.Allowed {
		t.Fatalf("denied a dry-run policy migration: %s", resp.Result.Message)
	}
}
