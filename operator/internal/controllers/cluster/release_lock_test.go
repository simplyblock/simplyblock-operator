// Releasing the lock an operation holds on its cluster, against a status the
// cluster's own reconciler is writing at the same time.
//
// The release is a compare-and-swap on purpose: it re-reads the cluster, checks
// the lock is still this operation's, and writes with an optimistic lock, so a
// release that lost a race never clears a lock somebody else now holds. Two
// operations running against one cluster with neither knowing is worse than a
// lock held a moment too long.
//
// Reporting the conflict as a reconciler error preserved that, and it is not the
// only thing that does. The cluster's status is written by its own reconciler on
// every pass, so a conflict here is ordinary rather than exceptional, and what it
// produced was a failed reconcile with a stack trace for an operation that had
// just succeeded. Retrying on a fresh read keeps the compare-and-swap — the check
// runs again on every attempt — and resolves what it can resolve itself.

package cluster

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

type errStaleCluster struct{}

func (errStaleCluster) Error() string {
	return "the object has been modified; please apply your changes to the latest version and try again"
}

// conflictingStatus refuses the first n status writes the way the API server
// does when something else has written the object since it was read.
func conflictingStatus(remaining *int) interceptor.Funcs {
	refuse := func(name string) error {
		*remaining--
		return apierrors.NewConflict(
			schema.GroupResource{
				Group:    simplyblockv1alpha2.GroupVersion.Group,
				Resource: "storageclusters",
			},
			name, errStaleCluster{})
	}
	return interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, c client.Client, subResource string,
			obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			if *remaining > 0 {
				return refuse(obj.GetName())
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	}
}

func lockedCluster(holder string) *simplyblockv1alpha2.StorageCluster {
	cluster := &simplyblockv1alpha2.StorageCluster{ObjectMeta: objectMeta("a-cluster")}
	cluster.Status.UUID = "cluster-uuid"
	cluster.Status.ActiveOpsRef = holder
	return cluster
}

func anOps(name string) *simplyblockv1alpha2.StorageClusterOps {
	return &simplyblockv1alpha2.StorageClusterOps{
		ObjectMeta: objectMeta(name),
		Spec: simplyblockv1alpha2.StorageClusterOpsSpec{
			ClusterRef: "a-cluster",
			Action:     simplyblockv1alpha2.StorageClusterOpsActionActivate,
		},
	}
}

func releaser(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) *StorageClusterOpsReconciler {
	t.Helper()
	return &StorageClusterOpsReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithStatusSubresource(
				&simplyblockv1alpha2.StorageCluster{},
				&simplyblockv1alpha2.StorageClusterOps{},
			).
			WithObjects(objects...).
			WithInterceptorFuncs(funcs).
			Build(),
		Scheme:   testScheme(t),
		Recorder: &recorder{},
	}
}

// A release that loses a race retries rather than failing the operation.
func TestTheLockReleaseRetriesAConflict(t *testing.T) {
	remaining := 1
	ops := anOps("a-cluster-activate")
	r := releaser(t, conflictingStatus(&remaining), lockedCluster("a-cluster-activate"), ops)

	if err := r.releaseLock(context.Background(), ops); err != nil {
		t.Fatalf("the release failed on a conflict it should have retried: %v", err)
	}
	if remaining != 0 {
		t.Error("the conflict was never raised, so this proves nothing")
	}

	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(lockedCluster("")), &cluster); err != nil {
		t.Fatalf("reading the cluster: %v", err)
	}
	if cluster.Status.ActiveOpsRef != "" {
		t.Errorf("the lock is still held by %q", cluster.Status.ActiveOpsRef)
	}
}

// The compare-and-swap survives the retry: a lock that has changed hands is not
// cleared, whatever this operation thinks it holds.
func TestTheReleaseNeverClearsSomebodyElsesLock(t *testing.T) {
	remaining := 0
	ops := anOps("a-cluster-activate")
	r := releaser(t, conflictingStatus(&remaining), lockedCluster("another-operation"), ops)

	if err := r.releaseLock(context.Background(), ops); err != nil {
		t.Fatalf("release: %v", err)
	}

	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(lockedCluster("")), &cluster); err != nil {
		t.Fatalf("reading the cluster: %v", err)
	}
	if cluster.Status.ActiveOpsRef != "another-operation" {
		t.Errorf("the lock now reads %q, want the other operation's", cluster.Status.ActiveOpsRef)
	}
}
