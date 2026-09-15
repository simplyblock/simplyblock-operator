// The control-plane surface this package needs, and the HTTP client that
// satisfies it.
//
// It is an interface so that a test can drive both controllers, including a
// whole drain and a whole relocation, without an HTTP server, and so that the
// one place a URL is spelled is the implementation below rather than scattered
// through the reconcilers. What it declares is the operator's question rather
// than the client's vocabulary: "suspend this node" rather than "POST this path."
//
// The endpoints are design-storagenode.md §12. Two of them are specified there as
// a `?watch=true` subscription, and the storage-node stream serves both: what
// remains here is the fallback each reader takes until its scope reports synced,
// plus the calls that have no streamed counterpart at all — the add, the delete,
// the five actions, and the volume list a drain classifies.
//
// A predicate over current state reads the same whether the state arrived by
// stream or by poll (design-crd-model.md §7.7), which is what lets a step's
// completion condition be written once.

package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/simplyblock/simplyblock-operator/internal/controllers/controlplane"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// NodeReading is one backend storage node as the control plane reports it. It is
// this package's own type rather than the client's, because every completion
// condition in the package is a predicate over it and the fields it needs are the
// ones named here.
type NodeReading struct {
	UUID               string `json:"id"`
	Status             string `json:"status"`
	ManagementIP       string `json:"mgmt_ip"`
	Health             bool   `json:"health_check"`
	Hostname           string `json:"hostname"`
	Uptime             string `json:"uptime"`
	DevicesCount       int32  `json:"device_count"`
	OnlineDevicesCount int32  `json:"online_device_count"`
	CPUCount           int32  `json:"cpu_spdk_count"`
	Memory             int64  `json:"spdk_mem"`
	Volumes            int32  `json:"lvols"`
	RPCPort            int32  `json:"rpc_port"`
	LvolPort           int32  `json:"lvol_subsys_port"`
	NVMeOFPort         int32  `json:"nvmf_port"`

	// FailureDomain is the group the control plane actually assigned. It is an
	// integer on the wire and a label on the object, so the status write renders
	// the digits: the control plane's own vocabulary is what it is, and the
	// operator does not invent a name the control plane never said (§3.3).
	FailureDomain int `json:"failure_domain"`
}

// The lifecycle values the control plane reports, in its own spelling. They are
// neither PascalCase nor an API enum for that reason: a backend status is the
// control plane's vocabulary rather than this group's (§7.3).
const (
	nodeStatusOnline     = "online"
	nodeStatusSuspended  = "suspended"
	nodeStatusOffline    = "offline"
	nodeStatusInCreation = "in_creation"
	nodeStatusInRestart  = "in_restart"
	nodeStatusActive     = "active"
)

// RestartParams are what the control plane's restart endpoint takes. Three
// actions use it and each fills a different subset: a plain Restart passes only
// the two flags, a Migrate passes the target's address and any drives being
// bound on it, and a HostMaintenance passes neither address nor drives because
// the node is coming back on the host it left.
type RestartParams struct {
	// NodeAddress is the per-pod DNS name the control plane resolves itself. A
	// name that does not resolve makes the restart fail inside the control plane,
	// whose response is to reset the node to offline, which is why the migration
	// blocks on the EndpointSlice before this is sent (§5.4).
	NodeAddress    string   `json:"node_address,omitempty"`
	Force          bool     `json:"force,omitempty"`
	ReattachVolume bool     `json:"reattach_volume,omitempty"`
	NewSsdPcie     []string `json:"new_ssd_pcie,omitempty"`
}

// ControlPlane is everything the two reconcilers in this package ask of the
// simplyblock control plane.
type ControlPlane interface {
	// AddNode adds every storage node of one worker at once and is not
	// idempotent, which is why the provisioning machine claims its slot in
	// Kubernetes before calling it (§4.2).
	AddNode(ctx context.Context, clusterID string, params utils.StorageNodeSetAddParams) error

	// StorageNodes are the cluster's nodes as the control plane reports them,
	// which is what adoption matches against and what the fallback of every
	// completion condition reads. It is the poll behind the storage-node stream.
	StorageNodes(ctx context.Context, clusterID string) ([]NodeReading, error)

	// StorageNode reads one node by its UUID. A node the control plane no longer
	// knows about reports false rather than an error, because a stored UUID that
	// has gone is the ordinary answer after a cluster was reset.
	StorageNode(ctx context.Context, clusterID, nodeID string) (NodeReading, bool, error)

	// The five node actions. Each must tolerate a repeat, because a step
	// recorded without its call having fired re-issues it (§7.2).
	Suspend(ctx context.Context, clusterID, nodeID string) error
	Resume(ctx context.Context, clusterID, nodeID string) error
	ShutdownNode(ctx context.Context, clusterID, nodeID string) error
	RestartNode(ctx context.Context, clusterID, nodeID string, params RestartParams) error

	// Promote is the migration's last control-plane call and the one that cannot
	// be undone: it activates the target host's devices, fails and migrates the
	// origin host's, starts a rebalance, and re-homes the logical volumes (§9).
	Promote(ctx context.Context, clusterID, nodeID string) error

	// RemoveNode is the drain's last step. A 404 is success: a node the control
	// plane no longer knows about is a node that has been removed, and a retry
	// after a lost response is the common way to arrive there (§8.2).
	RemoveNode(ctx context.Context, clusterID, nodeID string) error

	// StoragePools are the cluster's pools, which is the list a drain walks to
	// find the volumes on one node.
	StoragePools(ctx context.Context, clusterID string) ([]webapi.StoragePoolInfo, error)

	// PoolVolumes are the volumes of one pool. The control plane offers no
	// per-node volume list, so a drain reads every pool and keeps the volumes
	// whose node is the one being removed.
	PoolVolumes(ctx context.Context, clusterID, poolID string) ([]webapi.VolumeInfo, error)

	// DeleteVolume removes one volume, which verification does to the system
	// volumes a drain skipped. A 404 is success, for the reason RemoveNode's is.
	DeleteVolume(ctx context.Context, clusterID, poolID, volumeID string) error
}

// httpControlPlane is the ControlPlane the operator runs with: one method per
// endpoint of §12, over a client whose address comes from the ControlPlane object.
type httpControlPlane struct {
	// client is the startup client, built from the environment. It is what a
	// call uses until the ControlPlane publishes an endpoint.
	client *webapi.Client

	// resolve answers where the control plane is, per call. Nil means the
	// startup client is the only one.
	resolve controlplane.EndpointResolver

	// mu guards resolved, which is rebuilt when the published endpoint changes.
	mu       sync.Mutex
	resolved *webapi.Client
}

// NewControlPlane returns the HTTP-backed control-plane surface.
//
// The resolver may be nil, which is what a test passes: calls then go to the
// startup client and nothing reads a ControlPlane object.
func NewControlPlane(resolve controlplane.EndpointResolver) ControlPlane {
	return &httpControlPlane{client: webapi.NewClient(), resolve: resolve}
}

func (c *httpControlPlane) AddNode(
	ctx context.Context, clusterID string, params utils.StorageNodeSetAddParams,
) error {
	return c.post(ctx, fmt.Sprintf("/api/v2/clusters/%s/storage-nodes", clusterID), params)
}

func (c *httpControlPlane) StorageNodes(
	ctx context.Context, clusterID string,
) ([]NodeReading, error) {
	body, err := c.call(ctx, http.MethodGet,
		fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterID), nil)
	if err != nil {
		return nil, err
	}
	var nodes []NodeReading
	if err := json.Unmarshal(body, &nodes); err != nil {
		return nil, fmt.Errorf("read the storage node list: %w", err)
	}
	return nodes, nil
}

func (c *httpControlPlane) StorageNode(
	ctx context.Context, clusterID, nodeID string,
) (NodeReading, bool, error) {
	body, err := c.call(ctx, http.MethodGet,
		fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterID, nodeID), nil)
	var refusal *ControlPlaneError
	if errors.As(err, &refusal) && refusal.Status == http.StatusNotFound {
		return NodeReading{}, false, nil
	}
	if err != nil {
		return NodeReading{}, false, err
	}
	var node NodeReading
	if err := json.Unmarshal(body, &node); err != nil {
		return NodeReading{}, false, fmt.Errorf("read storage node %s: %w", nodeID, err)
	}
	return node, true, nil
}

func (c *httpControlPlane) Suspend(ctx context.Context, clusterID, nodeID string) error {
	return c.post(ctx, c.nodePath(clusterID, nodeID, "suspend"), nil)
}

func (c *httpControlPlane) Resume(ctx context.Context, clusterID, nodeID string) error {
	return c.post(ctx, c.nodePath(clusterID, nodeID, "resume"), nil)
}

func (c *httpControlPlane) ShutdownNode(ctx context.Context, clusterID, nodeID string) error {
	return c.post(ctx, c.nodePath(clusterID, nodeID, "shutdown"), nil)
}

func (c *httpControlPlane) RestartNode(
	ctx context.Context, clusterID, nodeID string, params RestartParams,
) error {
	return c.post(ctx, c.nodePath(clusterID, nodeID, "restart"), params)
}

func (c *httpControlPlane) Promote(ctx context.Context, clusterID, nodeID string) error {
	return c.post(ctx, c.nodePath(clusterID, nodeID, "promote"), nil)
}

// RemoveNode does not force the removal. The drain has already moved every
// volume off the node and verified there are none left, so a removal the control
// plane refuses is one of its own admission checks saying the cluster cannot
// afford to lose this node — which is an answer to report rather than to
// override (§8.2).
func (c *httpControlPlane) RemoveNode(ctx context.Context, clusterID, nodeID string) error {
	err := c.delete(ctx, fmt.Sprintf(
		"/api/v2/clusters/%s/storage-nodes/%s?force_remove=false", clusterID, nodeID))
	return ignoreGone(err)
}

func (c *httpControlPlane) StoragePools(
	ctx context.Context, clusterID string,
) ([]webapi.StoragePoolInfo, error) {
	body, err := c.call(ctx, http.MethodGet,
		fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterID), nil)
	if err != nil {
		return nil, err
	}
	var pools []webapi.StoragePoolInfo
	if err := json.Unmarshal(body, &pools); err != nil {
		return nil, fmt.Errorf("read the storage pool list: %w", err)
	}
	return pools, nil
}

func (c *httpControlPlane) PoolVolumes(
	ctx context.Context, clusterID, poolID string,
) ([]webapi.VolumeInfo, error) {
	body, err := c.call(ctx, http.MethodGet, fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/", clusterID, poolID), nil)
	if err != nil {
		return nil, err
	}
	var volumes []webapi.VolumeInfo
	if err := json.Unmarshal(body, &volumes); err != nil {
		return nil, fmt.Errorf("read the volumes of pool %s: %w", poolID, err)
	}
	return volumes, nil
}

func (c *httpControlPlane) DeleteVolume(
	ctx context.Context, clusterID, poolID, volumeID string,
) error {
	err := c.delete(ctx, fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/", clusterID, poolID, volumeID))
	return ignoreGone(err)
}

// nodePath is the action endpoint of one node. The segments keep the control
// plane's own lowercase spelling, because a URL is its vocabulary rather than
// this group's.
func (c *httpControlPlane) nodePath(clusterID, nodeID, action string) string {
	return fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/%s", clusterID, nodeID, action)
}

// post issues a call whose body is not read, which every action endpoint is.
func (c *httpControlPlane) post(ctx context.Context, path string, body any) error {
	_, err := c.call(ctx, http.MethodPost, path, body)
	return err
}

func (c *httpControlPlane) delete(ctx context.Context, path string) error {
	_, err := c.call(ctx, http.MethodDelete, path, nil)
	return err
}

// call performs one request and turns a non-2xx into a ControlPlaneError carrying
// the status and the body, which is what makes a refusal visible in
// `kubectl describe` without reading the operator's log.
func (c *httpControlPlane) call(
	ctx context.Context, method, path string, body any,
) ([]byte, error) {
	response, status, err := c.clientFor(ctx).Do(ctx, method, path, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if status >= 300 {
		return nil, &ControlPlaneError{Status: status, Body: string(response)}
	}
	return response, nil
}

// ignoreGone reads a 404 as success. A thing the control plane no longer knows
// about is the outcome the delete asked for, reached either by this call's own
// lost response or by somebody else (§8.2).
func ignoreGone(err error) error {
	var refusal *ControlPlaneError
	if errors.As(err, &refusal) && refusal.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// ControlPlaneError is a request the control plane refused. It carries the status
// and the whole body, because the body is where the control plane says why.
type ControlPlaneError struct {
	Status int
	Body   string
}

func (e *ControlPlaneError) Error() string {
	return fmt.Sprintf("the control plane answered %d: %s", e.Status, e.Body)
}

// clientFor is the client this call goes out on.
//
// The endpoint comes from ControlPlane.status.endpoint where the object has
// published one, which is what makes an external control plane reachable: the
// client built at startup resolves SIMPLYBLOCK_WEBAPI_BASE_URL or the in-cluster
// default, and neither is where somebody else's control plane is
// (design-controlplane.md §3.3).
//
// With no resolver, or with one that answers nothing, the startup client is used
// unchanged. That is what keeps this additive: a deployment whose ControlPlane
// has not published an endpoint behaves as it did before.
func (c *httpControlPlane) clientFor(ctx context.Context) *webapi.Client {
	if c.resolve == nil {
		return c.client
	}
	endpoint := c.resolve(ctx)
	if endpoint == "" || endpoint == c.client.BaseURL {
		return c.client
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved == nil || c.resolved.BaseURL != endpoint {
		c.resolved = webapi.NewClient(endpoint)
	}
	return c.resolved
}
