// The storage clusters a SimplyblockDriver declares in spec.remoteClusters:
// clusters whose control plane runs outside this Kubernetes cluster. The pass
// here checks each one, writes its entry into simplyblock-csi-secret-v2, removes
// the entries of items taken out of the spec, and reports each cluster's state.

package driver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	clustercontroller "github.com/simplyblock/simplyblock-operator/internal/controllers/cluster"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// remoteClusterSecretKey is where a credentials Secret holds the cluster secret.
	remoteClusterSecretKey = "cluster_secret"

	reasonRemoteClusterConnected          = "RemoteClusterConnected"
	reasonRemoteClusterUnreachable        = "RemoteClusterUnreachable"
	reasonRemoteClusterUnauthorized       = "RemoteClusterUnauthorized"
	reasonRemoteClusterCredentialsMissing = "RemoteClusterCredentialsMissing"
	reasonRemoteClusterConflict           = "RemoteClusterConflict"
	reasonRemoteClusterRemovedWithVolumes = "RemoteClusterRemovedWithVolumes"

	// namedVolumesMax bounds how many volumes the removal warning names.
	namedVolumesMax = 5
)

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch

// reconcileRemoteClusters runs the pass over spec.remoteClusters and writes the
// result into the status.
func (r *SimplyblockDriverReconciler) reconcileRemoteClusters(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver,
) error {
	if len(d.Spec.RemoteClusters) == 0 && len(d.Status.RemoteClusters) == 0 {
		return nil
	}
	local, err := r.localClusterIDs(ctx, d.Namespace)
	if err != nil {
		return err
	}
	previous := map[string]simplyblockv1alpha2.RemoteStorageClusterState{}
	for _, s := range d.Status.RemoteClusters {
		previous[s.ClusterID] = s.State
	}

	var statuses []simplyblockv1alpha2.RemoteStorageClusterStatus
	var entries []clustercontroller.CSIClusterEntry
	listed := map[string]bool{}
	for _, item := range d.Spec.RemoteClusters {
		listed[item.ClusterID] = true
		status, entry, err := r.checkRemoteCluster(ctx, d.Namespace, item, local[item.ClusterID])
		if err != nil {
			return err
		}
		if entry != nil {
			entries = append(entries, *entry)
		}
		if status.State != previous[item.ClusterID] {
			r.eventForRemoteState(d, status)
		}
		statuses = append(statuses, status)
	}

	var removed []string
	for id := range previous {
		if !listed[id] {
			removed = append(removed, id)
		}
	}
	slices.Sort(removed)

	if len(entries) > 0 || len(removed) > 0 {
		err := clustercontroller.EditCSICredentials(ctx, r.Client, d.Namespace, func(creds *clustercontroller.CSICredentials) {
			writeRemoteEntries(creds, entries, removed)
		})
		if err != nil {
			return fmt.Errorf("writing the remote cluster entries: %w", err)
		}
	}
	for _, id := range removed {
		if err := r.warnOnRemovedWithVolumes(ctx, d, id); err != nil {
			return err
		}
	}

	return r.writeStatus(ctx, d, func(status *simplyblockv1alpha2.SimplyblockDriverStatus) {
		status.RemoteClusters = statuses
		status.RemoteClustersTotal = int32(len(statuses))
		status.RemoteClustersConnected = 0
		for _, s := range statuses {
			if s.State == simplyblockv1alpha2.RemoteStorageClusterConnected {
				status.RemoteClustersConnected++
			}
		}
	})
}

// checkRemoteCluster resolves one item's credentials and asks its management
// API. It returns the entry to write, or nil when none may be written.
func (r *SimplyblockDriverReconciler) checkRemoteCluster(
	ctx context.Context, namespace string, item simplyblockv1alpha2.RemoteStorageCluster, isLocal bool,
) (simplyblockv1alpha2.RemoteStorageClusterStatus, *clustercontroller.CSIClusterEntry, error) {
	status := simplyblockv1alpha2.RemoteStorageClusterStatus{ClusterID: item.ClusterID}
	if isLocal {
		status.State = simplyblockv1alpha2.RemoteStorageClusterConflict
		status.Message = "a StorageCluster in this Kubernetes cluster already manages this cluster, and its entry stays"
		return status, nil, nil
	}

	secret, found, err := r.clusterSecret(ctx, namespace, item.CredentialsSecretRef.Name)
	if err != nil {
		return status, nil, err
	}
	if !found {
		status.State = simplyblockv1alpha2.RemoteStorageClusterCredentialsMissing
		status.Message = fmt.Sprintf("Secret %s has no %s key, or does not exist",
			item.CredentialsSecretRef.Name, remoteClusterSecretKey)
		return status, nil, nil
	}

	now := metav1.Now()
	status.LastProbeTime = &now
	status.State, status.Message = remoteStateOf(probeRemoteCluster(ctx, item.Endpoint, item.ClusterID, secret))
	return status, &clustercontroller.CSIClusterEntry{
		ClusterID:       item.ClusterID,
		ClusterEndpoint: item.Endpoint,
		ClusterSecret:   secret,
	}, nil
}

// clusterSecret reads the cluster secret from a credentials Secret.
func (r *SimplyblockDriverReconciler) clusterSecret(
	ctx context.Context, namespace, name string,
) (string, bool, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading the credentials Secret %s: %w", name, err)
	}
	value := strings.TrimSpace(string(secret.Data[remoteClusterSecretKey]))
	return value, value != "", nil
}

// probeRemoteCluster asks a cluster's management API for its storage nodes,
// which needs a reachable endpoint, an accepted cluster secret, and a cluster
// of that ID.
func probeRemoteCluster(ctx context.Context, endpoint, clusterID, secret string) error {
	api, err := atlascp.New(webapi.NewClient(endpoint).AtlasConfig(webapi.WithBearerToken(ctx, secret)))
	if err != nil {
		return err
	}
	_, err = api.ListStorageNodes(ctx, clusterID)
	return err
}

// remoteStateOf maps a probe's outcome to a state and its message.
func remoteStateOf(err error) (simplyblockv1alpha2.RemoteStorageClusterState, string) {
	if err == nil {
		return simplyblockv1alpha2.RemoteStorageClusterConnected, ""
	}
	var statusErr *atlascp.StatusError
	if errors.As(err, &statusErr) &&
		(statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden) {
		return simplyblockv1alpha2.RemoteStorageClusterUnauthorized,
			fmt.Sprintf("the management API refused the cluster secret (HTTP %d)", statusErr.StatusCode)
	}
	return simplyblockv1alpha2.RemoteStorageClusterUnreachable, err.Error()
}

// writeRemoteEntries upserts entries and drops the removed cluster IDs. A local
// entry is never replaced or dropped, because a StorageCluster owns it.
func writeRemoteEntries(creds *clustercontroller.CSICredentials, entries []clustercontroller.CSIClusterEntry, removed []string) {
	for _, entry := range entries {
		i := slices.IndexFunc(creds.Clusters, func(e clustercontroller.CSIClusterEntry) bool {
			return e.ClusterID == entry.ClusterID
		})
		switch {
		case i < 0:
			creds.Clusters = append(creds.Clusters, entry)
		case !creds.Clusters[i].Local:
			creds.Clusters[i] = entry
		}
	}
	creds.Clusters = slices.DeleteFunc(creds.Clusters, func(e clustercontroller.CSIClusterEntry) bool {
		return !e.Local && slices.Contains(removed, e.ClusterID)
	})
}

// removeRemoteEntries drops the entries this driver wrote, when it is deleted.
func (r *SimplyblockDriverReconciler) removeRemoteEntries(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver,
) error {
	ids := make([]string, 0, len(d.Status.RemoteClusters)+len(d.Spec.RemoteClusters))
	for _, s := range d.Status.RemoteClusters {
		ids = append(ids, s.ClusterID)
	}
	for _, item := range d.Spec.RemoteClusters {
		ids = append(ids, item.ClusterID)
	}
	if len(ids) == 0 {
		return nil
	}
	return clustercontroller.EditCSICredentials(ctx, r.Client, d.Namespace, func(creds *clustercontroller.CSICredentials) {
		writeRemoteEntries(creds, nil, ids)
	})
}

// localClusterIDs is the cluster IDs a StorageCluster wrote a local entry for.
func (r *SimplyblockDriverReconciler) localClusterIDs(ctx context.Context, namespace string) (map[string]bool, error) {
	ids := map[string]bool{}
	creds, err := r.readCSICredentials(ctx, namespace)
	if err != nil {
		return nil, err
	}
	for _, e := range creds.Clusters {
		if e.Local {
			ids[e.ClusterID] = true
		}
	}
	return ids, nil
}

// warnOnRemovedWithVolumes reports a removed cluster that PersistentVolumes of
// this driver still name. The removal goes ahead: the volumes keep their data,
// and adding the item back makes them usable again.
func (r *SimplyblockDriverReconciler) warnOnRemovedWithVolumes(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver, clusterID string,
) error {
	var pvs corev1.PersistentVolumeList
	if err := r.List(ctx, &pvs); err != nil {
		return fmt.Errorf("listing PersistentVolumes for removed cluster %s: %w", clusterID, err)
	}
	driverName := names(d).csiDriver
	var volumes []string
	for _, pv := range pvs.Items {
		csi := pv.Spec.CSI
		if csi == nil || csi.Driver != driverName {
			continue
		}
		if h, ok := lvol.ParseHandle(lvol.VolumeHandle(csi.VolumeHandle)); ok && h.ClusterID == clusterID {
			volumes = append(volumes, pv.Name)
		}
	}
	if len(volumes) == 0 {
		return nil
	}
	slices.Sort(volumes)
	shown := volumes[:min(len(volumes), namedVolumesMax)]
	r.event(d, corev1.EventTypeWarning, reasonRemoteClusterRemovedWithVolumes, fmt.Sprintf(
		"cluster %s was removed from spec.remoteClusters while %d PersistentVolume(s) still use it (%s); "+
			"they cannot be attached or deleted until it is added back",
		clusterID, len(volumes), strings.Join(shown, ", ")))
	return nil
}

// eventForRemoteState records a cluster's arrival in a state.
func (r *SimplyblockDriverReconciler) eventForRemoteState(
	d *simplyblockv1alpha2.SimplyblockDriver, s simplyblockv1alpha2.RemoteStorageClusterStatus,
) {
	reason, eventType := map[simplyblockv1alpha2.RemoteStorageClusterState]string{
		simplyblockv1alpha2.RemoteStorageClusterConnected:          reasonRemoteClusterConnected,
		simplyblockv1alpha2.RemoteStorageClusterUnreachable:        reasonRemoteClusterUnreachable,
		simplyblockv1alpha2.RemoteStorageClusterUnauthorized:       reasonRemoteClusterUnauthorized,
		simplyblockv1alpha2.RemoteStorageClusterCredentialsMissing: reasonRemoteClusterCredentialsMissing,
		simplyblockv1alpha2.RemoteStorageClusterConflict:           reasonRemoteClusterConflict,
	}[s.State], corev1.EventTypeWarning
	if s.State == simplyblockv1alpha2.RemoteStorageClusterConnected {
		eventType = corev1.EventTypeNormal
	}
	message := fmt.Sprintf("cluster %s is %s", s.ClusterID, s.State)
	if s.Message != "" {
		message += ": " + s.Message
	}
	r.event(d, eventType, reason, message)
}
