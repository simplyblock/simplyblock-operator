// The driver reconcile runs the metadata server's state volume, allowed hosts,
// and scale for each storage cluster's StatefulSet (design-pnfs-mds-standby.md
// §4.1, §5.4, P0-2).

package driver

import (
	"context"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/mdsstate"
	"github.com/simplyblock/simplyblock-operator/internal/mdsstate/mdsstatetest"
)

type noSwitchover struct{}

func (noSwitchover) ActiveOrdinal(context.Context, *appsv1.StatefulSet) (int32, bool, error) {
	return 0, false, nil
}

func (noSwitchover) RequestSwitchover(context.Context, *appsv1.StatefulSet, int32) error { return nil }

const coordinatorStatefulSetUID = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"

func coordinatorFixture(t *testing.T, standbys int32, replicas int32, api *mdsstatetest.API, extra ...client.Object,
) (*SimplyblockDriverReconciler, client.Client, *appsv1.StatefulSet) {
	t.Helper()
	d := withMDS(testDriver("simplyblock"))
	d.Spec.PNFS.MDS.Standbys = ptr.To(standbys)
	_, sts := mdsObjects(t, d)
	sts.UID = coordinatorStatefulSetUID
	sts.Labels = map[string]string{MDSClusterLabel: testClusterID}
	sts.Spec.Replicas = ptr.To(replicas)

	scheme := reconcilerScheme(t)
	objs := append([]client.Object{d, sts}, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(d).Build()
	r := &SimplyblockDriverReconciler{
		Client: c, Scheme: scheme, Snapshots: fixedSnapshotAPI{served: true}, Recorder: events.NewFakeRecorder(50),
		MDSState: &mdsstate.Coordinator{
			APIFor:     func(context.Context, string) (mdsstate.VolumeAPI, error) { return api, nil },
			Switchover: noSwitchover{},
		},
	}
	r.MDSState.Client = c
	return r, c, sts
}

func nfsExportOf(name string, volume lvol.VolumeHandle) *simplyblockv1alpha2.NFSExport {
	return &simplyblockv1alpha2.NFSExport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: metav1.Now()},
		Spec:       simplyblockv1alpha2.NFSExportSpec{VolumeRef: string(volume)},
	}
}

func reconcileMDSDriver(t *testing.T, r *SimplyblockDriverReconciler) {
	t.Helper()
	d := withMDS(testDriver("simplyblock"))
	if _, err := r.Reconcile(context.Background(), requestFor(d)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// A standby raised from 0 to 1 scales the StatefulSet, the first export's pool
// gets the state volume, and every pod's host is allowed on the export.
func TestADriverReconcileRunsTheMetadataServerCoordinator(t *testing.T) {
	api := mdsstatetest.New()
	export := api.Add("export", "nqn.client")
	r, c, sts := coordinatorFixture(t, 1, 1, api, nfsExportOf("e1", export))

	reconcileMDSDriver(t, r)

	var got appsv1.StatefulSet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &got); err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *got.Spec.Replicas)
	}
	if got.Annotations[mdsstate.StateVolumeAnnotation] == "" {
		t.Errorf("no state volume recorded: %v", got.Annotations)
	}
	host, _ := mdsstate.HostNQN(coordinatorStatefulSetUID, 1)
	if !slices.Contains(api.HostsOf(export), host) {
		t.Errorf("export hosts = %v, want ordinal 1's %s", api.HostsOf(export), host)
	}
}

// An export of another storage cluster is none of this StatefulSet's volumes.
func TestADriverReconcileLeavesAnotherClustersExportsAlone(t *testing.T) {
	api := mdsstatetest.New()
	other := lvol.VolumeHandle("7c3e9a10-0000-4000-8000-000000000000:" + mdsstatetest.Pool + ":00000000-0000-4000-8000-000000000042")
	api.Volumes[other] = &mdsstatetest.Volume{Vol: lvol.Volume{ID: other, Name: "other"}, Hosts: []string{"nqn.client"}}
	r, _, _ := coordinatorFixture(t, 1, 2, api, nfsExportOf("e1", other))

	reconcileMDSDriver(t, r)

	if got := api.HostsOf(other); len(got) != 1 {
		t.Errorf("another cluster's export got hosts: %v", got)
	}
	if api.Creates != 0 {
		t.Errorf("%d volumes created for a cluster with no export", api.Creates)
	}
}

// Deleting the StatefulSet deletes the state volume through the finalizer.
func TestADriverReconcileFinalizesADeletedStatefulSet(t *testing.T) {
	api := mdsstatetest.New()
	state := api.Add("state")
	d := withMDS(testDriver("simplyblock"))
	_, sts := mdsObjects(t, d)
	sts.UID = coordinatorStatefulSetUID
	sts.Labels = map[string]string{MDSClusterLabel: testClusterID}
	sts.Annotations[mdsstate.StateVolumeAnnotation] = string(state)
	sts.Finalizers = []string{mdsstate.StateVolumeFinalizer}
	now := metav1.Now()
	sts.DeletionTimestamp = &now

	scheme := reconcilerScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d, sts).WithStatusSubresource(d).Build()
	r := &SimplyblockDriverReconciler{
		Client: c, Scheme: scheme, Snapshots: fixedSnapshotAPI{served: true},
		MDSState: &mdsstate.Coordinator{
			Client: c,
			APIFor: func(context.Context, string) (mdsstate.VolumeAPI, error) { return api, nil },
		},
	}
	reconcileMDSDriver(t, r)

	if _, ok := api.Volumes[state]; ok {
		t.Error("the state volume was not deleted")
	}
	var got appsv1.StatefulSet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &got); err == nil &&
		slices.Contains(got.Finalizers, mdsstate.StateVolumeFinalizer) {
		t.Errorf("finalizers = %v, want the state volume's removed", got.Finalizers)
	}
}

// The takeover holds a promotion for a volume whose pods' hosts are not all
// allowed yet, so each pass hands the coordinator's host result to it.
func TestADriverReconcileReportsTheAllowedHostsToTheTakeover(t *testing.T) {
	api := mdsstatetest.New()
	export := api.Add("export", "nqn.client")
	r, _, _ := coordinatorFixture(t, 1, 2, api, nfsExportOf("e1", export))
	var got []mdsstate.HostResult
	r.OnHosts = func(h mdsstate.HostResult) { got = append(got, h) }

	reconcileMDSDriver(t, r)

	if len(got) == 0 {
		t.Fatal("the host result never reached the takeover")
	}
	if !got[len(got)-1].Allowed(export) {
		t.Errorf("export %s is not reported allowed: %+v", export, got[len(got)-1])
	}
}
