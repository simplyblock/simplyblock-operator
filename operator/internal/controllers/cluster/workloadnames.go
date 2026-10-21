// How a cluster comes by its workload names (design-superclusters.md §6.1),
// which the cluster reconciler records before anything is built from them: the
// constant names for the first cluster in a namespace and for one created before
// the record existed, the cluster's own names for every other, and the check
// that the control plane kept what it was sent.
//
// It lives beside the creation machine because the names are decided on the
// creation path and checked where it ends. The workload reconciler only reads
// the record.

package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	atlaskube "github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// chooseWorkloadNames decides the names of a cluster that has none recorded.
//
// A cluster with a backend cluster was created before the record, and its nodes
// were registered under the constant names, which are the only ones it can
// have. A member is reached through its supercluster's Services with a suffix
// of its own (§6.2). A new standalone cluster takes the constant names unless
// another cluster in its namespace recorded them or holds the Service that
// carries them.
func (r *StorageClusterReconciler) chooseWorkloadNames(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (atlaskube.WorkloadNames, error) {
	constant := atlaskube.ConstantWorkloadNames()
	if cluster.Status.UUID != "" {
		return constant, nil
	}
	if cluster.Spec.SuperclusterRef != "" {
		return atlaskube.MemberWorkloadNames(cluster.Spec.SuperclusterRef, cluster.Name), nil
	}

	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(cluster.Namespace)); err != nil {
		return atlaskube.WorkloadNames{}, fmt.Errorf("list the clusters of namespace %s: %w", cluster.Namespace, err)
	}
	for i := range clusters.Items {
		other := &clusters.Items[i]
		if other.Name == cluster.Name {
			continue
		}
		if names, recorded := utils.WorkloadNamesOf(other); recorded && names.IsConstant() {
			return atlaskube.DerivedWorkloadNames(cluster.Name), nil
		}
	}

	held, err := r.constantServiceHeldByAnother(ctx, cluster)
	if err != nil {
		return atlaskube.WorkloadNames{}, err
	}
	if held {
		return atlaskube.DerivedWorkloadNames(cluster.Name), nil
	}
	return constant, nil
}

// constantServiceHeldByAnother reports whether the storage-node API Service
// with the constant name is controlled by something other than this cluster.
// It is what decides between two clusters that recorded the constant names at
// once: the workload reconciler never takes a Service another object controls,
// so the cluster that finds it held has lost.
func (r *StorageClusterReconciler) constantServiceHeldByAnother(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (bool, error) {
	var service corev1.Service
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: atlaskube.ConstantNodeAPIService}
	if err := r.Get(ctx, key, &service); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read Service %s: %w", key.Name, err)
	}
	owner := metav1.GetControllerOf(&service)
	return owner != nil && owner.UID != cluster.UID, nil
}

// recordConstantWorkloadNames gives a cluster created before the record its
// names, which are the constant ones its nodes were registered under.
func (r *StorageClusterReconciler) recordConstantWorkloadNames(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	return r.writeStatus(ctx, cluster, func(status *simplyblockv1alpha2.StorageClusterStatus) {
		if status.WorkloadNames == nil {
			status.WorkloadNames = utils.WorkloadNamesStatus(atlaskube.ConstantWorkloadNames())
		}
	})
}

// settleWorkloadNames runs before the backend cluster is created. A cluster
// that recorded the constant names and finds their Service held by another
// cluster lost the race for them, and takes its own names while nothing has
// been built under the constant ones. It reports whether the record changed.
func (r *StorageClusterReconciler) settleWorkloadNames(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (bool, error) {
	names, _ := utils.WorkloadNamesOf(cluster)
	if !names.IsConstant() {
		return false, nil
	}
	held, err := r.constantServiceHeldByAnother(ctx, cluster)
	if err != nil || !held {
		return false, err
	}
	derived := atlaskube.DerivedWorkloadNames(cluster.Name)
	err = r.writeStatus(ctx, cluster, func(status *simplyblockv1alpha2.StorageClusterStatus) {
		status.WorkloadNames = utils.WorkloadNamesStatus(derived)
		status.Message = fmt.Sprintf(
			"Service %s belongs to another cluster; this cluster is reached through %s instead",
			atlaskube.ConstantNodeAPIService, derived.NodeAPIService)
	})
	return err == nil, err
}

// backendMissesWorkloadNames reports, and says on the cluster, that the control
// plane's record of the cluster does not carry the names it was created with. A
// control plane that predates the names creates every cluster under the
// constant ones, so a cluster with its own names is unreachable there, and it
// is held rather than finished.
func (r *StorageClusterReconciler) backendMissesWorkloadNames(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, found adoption,
) (bool, error) {
	names, _ := utils.WorkloadNamesOf(cluster)
	if utils.BackendHolds(found.Names, names) {
		return false, nil
	}
	message := fmt.Sprintf(
		"the control plane's cluster %s is not reached through %s, the Service this cluster "+
			"recorded; the control plane does not keep a cluster's workload names and has to be "+
			"upgraded before a second cluster can share namespace %s",
		found.UUID, names.NodeAPIService, cluster.Namespace)
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
		WorkloadNamesNotRecorded, WorkloadNamesNotRecorded, "%s", message)
	return true, r.reportUnavailable(ctx, cluster, message)
}

// backendWorkloadNamesOf is the part of the cluster's names the create body
// carries.
func backendWorkloadNamesOf(cluster *simplyblockv1alpha2.StorageCluster) webapi.BackendWorkloadNames {
	names, _ := utils.WorkloadNamesOf(cluster)
	return utils.BackendWorkloadNames(names)
}
