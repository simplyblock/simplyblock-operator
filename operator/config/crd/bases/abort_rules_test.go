// Whether an Ops kind refuses withdrawing spec.abort. Abort starts an unwind,
// and an unwind withdrawn midway resumes the forward steps against a partly
// undone operation, so every Ops kind that has abort makes it one-way.

package bases

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestAbortCannotBeWithdrawn(t *testing.T) {
	kinds := []struct {
		kind     string
		aborted  client.Object
		withdraw func(client.Object)
	}{
		{"ControlPlaneOps", &simplyblockv1alpha2.ControlPlaneOps{Spec: simplyblockv1alpha2.ControlPlaneOpsSpec{
			ControlPlaneRef: "simplyblock", Action: simplyblockv1alpha2.ControlPlaneOpsActionRestart, Abort: true,
		}}, func(o client.Object) { o.(*simplyblockv1alpha2.ControlPlaneOps).Spec.Abort = false }},
		{"OperatorOps", &simplyblockv1alpha2.OperatorOps{Spec: simplyblockv1alpha2.OperatorOpsSpec{
			Action: simplyblockv1alpha2.OperatorOpsActionDiscover, Abort: true,
			Discover: &simplyblockv1alpha2.DiscoverSpec{Workers: []string{"worker-1"}},
		}}, func(o client.Object) { o.(*simplyblockv1alpha2.OperatorOps).Spec.Abort = false }},
		{"PersistentVolumeOps", &simplyblockv1alpha2.PersistentVolumeOps{Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
			PersistentVolumeName: "pv-1", Action: simplyblockv1alpha2.PersistentVolumeOpsActionMigrate, Abort: true,
			Migrate: &simplyblockv1alpha2.MigrateVolumeSpec{
				TargetNodeRef: simplyblockv1alpha2.StorageNodeReference{Namespace: "default", Name: "node-1"},
			},
		}}, func(o client.Object) { o.(*simplyblockv1alpha2.PersistentVolumeOps).Spec.Abort = false }},
		{"StorageBackupOps", &simplyblockv1alpha2.StorageBackupOps{Spec: simplyblockv1alpha2.StorageBackupOpsSpec{
			ClusterRef: simplyblockv1alpha2.NamespacedDnsReference{Name: "cluster-1"},
			BackupRef:  simplyblockv1alpha2.NamespacedReference{Name: "backup-1"},
			Action:     simplyblockv1alpha2.StorageBackupOpsActionRestore, Abort: true,
			Restore: &simplyblockv1alpha2.RestoreSpec{ClaimName: "restored", TargetPool: "pool-1"},
		}}, func(o client.Object) { o.(*simplyblockv1alpha2.StorageBackupOps).Spec.Abort = false }},
		{"StorageClusterOps", clusterOps(simplyblockv1alpha2.StorageClusterOpsActionActivate,
			func(s *simplyblockv1alpha2.StorageClusterOpsSpec) { s.Abort = true }),
			asClusterOps(func(s *simplyblockv1alpha2.StorageClusterOpsSpec) { s.Abort = false })},
		{"StorageDeviceOps", &simplyblockv1alpha2.StorageDeviceOps{Spec: simplyblockv1alpha2.StorageDeviceOpsSpec{
			DeviceRef: "device-1", Action: simplyblockv1alpha2.StorageDeviceOpsActionRestart, Abort: true,
		}}, func(o client.Object) { o.(*simplyblockv1alpha2.StorageDeviceOps).Spec.Abort = false }},
		{"StorageNodeOps", nodeOps(simplyblockv1alpha2.StorageNodeOpsActionShutdown,
			func(s *simplyblockv1alpha2.StorageNodeOpsSpec) { s.Abort = true }),
			asNodeOps(func(s *simplyblockv1alpha2.StorageNodeOpsSpec) { s.Abort = false })},
	}
	cases := make([]transition, 0, len(kinds))
	for _, k := range kinds {
		cases = append(cases, transition{
			name:   "withdrawing the abort of " + k.kind,
			create: k.aborted,
			mutate: k.withdraw,
			denied: "spec.abort cannot be withdrawn",
		})
	}
	runTransitions(t, cases)
}
