// Optional fields an update may not set when they were left unset at
// creation. The generated rule for an immutable optional field refuses a change
// and a removal, but runs only where an old value exists, so a first
// assignment after creation is admitted; these fields are read once, early,
// and a value set later is ignored or retargets the work midway.

package bases

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// claimPolicy is a policy over a claim selector, created with no schedule and
// no retention.
func claimPolicy() *simplyblockv1alpha2.StorageBackupPolicy {
	return &simplyblockv1alpha2.StorageBackupPolicy{Spec: simplyblockv1alpha2.StorageBackupPolicySpec{
		ClusterRef:    "cluster-1",
		ClaimSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"backup": "daily"}},
	}}
}

func asPolicySpec(f func(*simplyblockv1alpha2.StorageBackupPolicySpec)) func(client.Object) {
	return func(o client.Object) { f(&o.(*simplyblockv1alpha2.StorageBackupPolicy).Spec) }
}

func TestABackupPolicyScheduleCannotBeAddedLater(t *testing.T) {
	runTransitions(t, []transition{{
		name:   "scheduling a policy created without a schedule",
		create: claimPolicy(),
		mutate: asPolicySpec(func(s *simplyblockv1alpha2.StorageBackupPolicySpec) { s.Schedule = "15m,4 60m,11 24h,7" }),
		denied: "spec.schedule cannot be set after creation",
	}})
}

func TestABackupPolicyMaxVersionsCannotBeAddedLater(t *testing.T) {
	runTransitions(t, []transition{{
		name:   "capping the versions of a policy created without a cap",
		create: claimPolicy(),
		mutate: asPolicySpec(func(s *simplyblockv1alpha2.StorageBackupPolicySpec) { s.MaxVersions = ptr.To(int32(5)) }),
		denied: "spec.maxVersions cannot be set after creation",
	}})
}

func TestABackupPolicyMaxAgeCannotBeAddedLater(t *testing.T) {
	runTransitions(t, []transition{{
		name:   "capping the age of a policy created without a cap",
		create: claimPolicy(),
		mutate: asPolicySpec(func(s *simplyblockv1alpha2.StorageBackupPolicySpec) { s.MaxAge = "30d" }),
		denied: "spec.maxAge cannot be set after creation",
	}})
}

func TestAVolumeOperationsCreatorCannotBeAddedLater(t *testing.T) {
	migrate := &simplyblockv1alpha2.PersistentVolumeOps{Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
		PersistentVolumeName: "pv-1", Action: simplyblockv1alpha2.PersistentVolumeOpsActionMigrate,
		Migrate: &simplyblockv1alpha2.MigrateVolumeSpec{
			TargetNodeRef: simplyblockv1alpha2.StorageNodeReference{Namespace: "default", Name: "node-1"},
		},
	}}
	runTransitions(t, []transition{{
		name:   "naming a creator on a hand-written migration",
		create: migrate,
		mutate: func(o client.Object) {
			o.(*simplyblockv1alpha2.PersistentVolumeOps).Spec.CreatorRef = &simplyblockv1alpha2.CreatorReference{
				Kind: "StorageNodeOps", Namespace: "default", Name: "remove-node-1", UID: "0b0e6c1e-0000-4000-8000-000000000001",
			}
		},
		denied: "spec.creatorRef cannot be set after creation",
	}})
}
