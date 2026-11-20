// The mds-proxy's listener ports for storage clusters whose nodes are not in
// this Kubernetes cluster. A managed consumer has no StorageNode objects, so
// the ports come from the control plane of every cluster the CSI driver is
// configured for in simplyblock-csi-secret-v2.

package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	atlascp "github.com/simplyblock/atlas/controlplane"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	clustercontroller "github.com/simplyblock/simplyblock-operator/internal/controllers/cluster"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// csiCredentialsSecret and csiCredentialsKey are where the CSI driver reads
	// the clusters it serves, and with them their endpoints and secrets.
	csiCredentialsSecret = "simplyblock-csi-secret-v2"
	csiCredentialsKey    = "secret.json"

	reasonListenerPortsUnknown = "ListenerPortsUnknown"
)

// remoteListenerPorts is the LVS port of every storage node in the clusters
// simplyblock-csi-secret-v2 lists and this operator does not manage, asked of
// each cluster's control plane with that cluster's secret.
func (r *SimplyblockDriverReconciler) remoteListenerPorts(ctx context.Context, namespace string) ([]int32, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: namespace, Name: csiCredentialsSecret}
	if err := r.Get(ctx, key, &secret); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	raw, ok := secret.Data[csiCredentialsKey]
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	var creds clustercontroller.CSICredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, fmt.Errorf("reading %s: %w", csiCredentialsSecret, err)
	}

	var ports []int32
	var errs []error
	for _, e := range creds.Clusters {
		// A cluster this operator manages has StorageNode objects, and its
		// ports are read from them.
		if e.Local || e.ClusterID == "" || e.ClusterEndpoint == "" {
			continue
		}
		found, err := clusterListenerPorts(ctx, e)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ports = append(ports, found...)
	}
	return ports, errors.Join(errs...)
}

// clusterListenerPorts asks one cluster's control plane for its nodes' ports.
func clusterListenerPorts(ctx context.Context, e clustercontroller.CSIClusterEntry) ([]int32, error) {
	cfg := webapi.NewClient(e.ClusterEndpoint).AtlasConfig(webapi.WithBearerToken(ctx, e.ClusterSecret))
	api, err := atlascp.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("control-plane client for cluster %s: %w", e.ClusterID, err)
	}
	nodes, err := api.ListStorageNodes(ctx, e.ClusterID)
	if err != nil {
		return nil, fmt.Errorf("storage nodes of cluster %s at %s: %w", e.ClusterID, e.ClusterEndpoint, err)
	}
	var ports []int32
	for _, n := range nodes {
		if n.LvolPort > 0 && n.LvolPort <= 65535 {
			ports = append(ports, int32(n.LvolPort))
		}
	}
	return ports, nil
}

// publishedListenerPorts is what the ports ConfigMap carries now. It stands in
// for a control plane that did not answer, so a metadata server already
// serving through those ports keeps them.
func (r *SimplyblockDriverReconciler) publishedListenerPorts(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver,
) ([]int32, error) {
	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: d.Namespace, Name: mdsProxyPortsName(d)}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var ports []int32
	for _, line := range strings.Fields(cm.Data[mdsProxyPortsKey]) {
		if p, err := strconv.ParseInt(line, 10, 32); err == nil && p > 0 {
			ports = append(ports, int32(p))
		}
	}
	return ports, nil
}
