// How a cluster comes by its workload names (design-superclusters.md §6.1),
// which the cluster reconciler records before anything is built from them: the
// constant names for a cluster whose nodes were registered before the record
// existed, the cluster's own names for every new one, and the check that the
// control plane kept what it was sent.
//
// It lives beside the creation machine because the names are decided on the
// creation path and checked where it ends. The workload reconciler only reads
// the record.

package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	atlascp "github.com/simplyblock/atlas/controlplane"
	atlaskube "github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// chooseWorkloadNames decides the names of a cluster that has none recorded.
//
// A new cluster takes names of its own, a member its supercluster's with a
// suffix of its own (§6.2), and nothing in a namespace holds names another
// cluster has to avoid. Two kinds of cluster run under the constant names
// instead, because their nodes were registered under them before the record
// existed: a cluster with a backend cluster, which was created before it, and
// one adopted from a Helm deployment through its upgrade Secret.
func (r *StorageClusterReconciler) chooseWorkloadNames(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (atlaskube.WorkloadNames, error) {
	if cluster.Status.UUID != "" {
		return atlaskube.ConstantWorkloadNames(), nil
	}
	if _, adopting, err := r.upgradeSecret(ctx, cluster); err != nil {
		return atlaskube.WorkloadNames{}, err
	} else if adopting {
		return atlaskube.ConstantWorkloadNames(), nil
	}
	if cluster.Spec.SuperclusterRef != "" {
		return atlaskube.MemberWorkloadNames(cluster.Spec.SuperclusterRef, cluster.Name), nil
	}
	return atlaskube.DerivedWorkloadNames(cluster.Name), nil
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

// backendMissesWorkloadNames reports, and says on the cluster, that the control
// plane's record of the cluster does not carry the names it was created with. A
// control plane that predates the names creates every cluster under the
// constant ones, so a new cluster, which has names of its own, is unreachable
// there, and it is held rather than finished.
func (r *StorageClusterReconciler) backendMissesWorkloadNames(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, found adoption,
) (bool, error) {
	names, _ := utils.WorkloadNamesOf(cluster)
	if utils.BackendHolds(found.Names, names) {
		return false, nil
	}
	message := fmt.Sprintf(
		"the control plane's cluster %s is not reached through %s, the Service this cluster "+
			"recorded: the control plane does not keep a cluster's workload names, and has to be "+
			"upgraded before it can run a cluster created by this operator",
		found.UUID, names.NodeAPIService)
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
		WorkloadNamesNotRecorded, WorkloadNamesNotRecorded, "%s", message)
	return true, r.reportUnavailable(ctx, cluster, message)
}

// backendWorkloadNamesOf is the part of the cluster's names the create body
// carries.
func backendWorkloadNamesOf(cluster *simplyblockv1alpha2.StorageCluster) atlascp.ClusterWorkloadNames {
	names, _ := utils.WorkloadNamesOf(cluster)
	return utils.BackendWorkloadNames(names)
}

// recordHeldBackend writes the backend cluster of a cluster held for its names
// into the cluster's Secret: its UUID, and the credential the creation
// returned or an earlier pass recorded. The status does not record the backend
// cluster, because the steady state must not run on it, so the Secret is what
// lets deleting the StorageCluster delete it too.
func (r *StorageClusterReconciler) recordHeldBackend(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, found adoption,
) error {
	if secret, err := r.credentialFor(ctx, cluster, found); err == nil {
		found.Secret = secret
	}
	return r.writeClusterSecret(ctx, cluster, found)
}

// recordedBackendUUID is the backend cluster the cluster's Secret names, and
// empty when there is no Secret or it names none.
func (r *StorageClusterReconciler) recordedBackendUUID(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) string {
	var secret corev1.Secret
	key := types.NamespacedName{Name: clusterSecretName(cluster), Namespace: cluster.Namespace}
	if err := r.Get(ctx, key, &secret); err != nil {
		return ""
	}
	return string(secret.Data["uuid"])
}
