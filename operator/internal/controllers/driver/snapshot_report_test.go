// What status.snapshotSupport reports about a cluster's snapshot support
// (design-simplyblockdriver.md §4.1, test plan rows U-39 and U-108 to U-110).
//
// Each test is a Reconcile against a fake client, because what the rows assert is
// what a reconcile leaves in the cluster and in status.

package driver

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// reportFixture is a reconciler on a cluster that serves the snapshot API or not.
func reportFixture(t *testing.T, served bool) (*SimplyblockDriverReconciler,
	client.Client, *simplyblockv1alpha2.SimplyblockDriver) {
	t.Helper()
	scheme := reconcilerScheme(t)
	d := testDriver("simplyblock")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d).WithStatusSubresource(d).Build()
	return &SimplyblockDriverReconciler{
		Client: c, Scheme: scheme, Snapshots: fixedSnapshotAPI{served: served},
	}, c, d
}

func reconcileOnce(t *testing.T, r *SimplyblockDriverReconciler, d *simplyblockv1alpha2.SimplyblockDriver) {
	t.Helper()
	if _, err := r.Reconcile(t.Context(), requestFor(d)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func snapshotSupportOf(t *testing.T, c client.Client, d *simplyblockv1alpha2.SimplyblockDriver,
) simplyblockv1alpha2.SnapshotSupportOrigin {
	t.Helper()
	var got simplyblockv1alpha2.SimplyblockDriver
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(d), &got); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	return got.Status.SnapshotSupport
}

// U-108: the cluster that cannot serve snapshots is told so, rather than left to
// find out when a VolumeSnapshot never becomes ready.
func TestACrdlessClusterReportsMissing(t *testing.T) {
	r, c, d := reportFixture(t, false)

	reconcileOnce(t, r, d)

	if got := snapshotSupportOf(t, c, d); got != simplyblockv1alpha2.SnapshotSupportOriginMissing {
		t.Errorf("status.snapshotSupport = %q, want Missing", got)
	}
}

// U-39: a served API reads Detected. The operator looks for the API and nothing
// else, because a snapshot-controller can run under any name in any namespace.
func TestAServedAPIReportsDetected(t *testing.T) {
	r, c, d := reportFixture(t, true)

	reconcileOnce(t, r, d)

	if got := snapshotSupportOf(t, c, d); got != simplyblockv1alpha2.SnapshotSupportOriginDetected {
		t.Errorf("status.snapshotSupport = %q, want Detected", got)
	}
}

// U-109: status follows the cluster: an API that appears moves Missing to
// Detected.
func TestMissingBecomesDetectedWhenTheAPIAppears(t *testing.T) {
	r, c, d := reportFixture(t, false)
	reconcileOnce(t, r, d)
	if got := snapshotSupportOf(t, c, d); got != simplyblockv1alpha2.SnapshotSupportOriginMissing {
		t.Fatalf("status.snapshotSupport = %q, want Missing first", got)
	}

	r.Snapshots = fixedSnapshotAPI{served: true}
	reconcileOnce(t, r, d)

	if got := snapshotSupportOf(t, c, d); got != simplyblockv1alpha2.SnapshotSupportOriginDetected {
		t.Errorf("status.snapshotSupport = %q, want Detected once the API is served", got)
	}
}

// U-110: a deployment that turned snapshots off reports nothing.
func TestSnapshotsDisabledReportsNothing(t *testing.T) {
	r, c, d := reportFixture(t, false)
	off := false
	d.Spec.EnableVolumeSnapshots = &off
	if err := c.Update(t.Context(), d); err != nil {
		t.Fatalf("update: %v", err)
	}

	reconcileOnce(t, r, d)

	if got := snapshotSupportOf(t, c, d); got != "" {
		t.Errorf("status.snapshotSupport = %q for a deployment that disabled snapshots", got)
	}
}
