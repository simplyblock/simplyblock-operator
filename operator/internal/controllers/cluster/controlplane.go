// The control-plane surface this package needs, and the HTTP client that
// satisfies it.
//
// It is an interface so that a test can drive both controllers, including the
// whole rolling-restart walk, without an HTTP server, and so that the one place
// a URL is spelled is the implementation below rather than scattered through
// the reconcilers. What it declares is the operator's question rather than the
// client's vocabulary: "shut this node down" rather than "POST this path."
//
// The endpoints are design-storagecluster.md §9. Three of them are specified
// there as `?watch=true` subscriptions, and all three are served by the
// control-plane informer: the cluster stream, the storage-node stream, and the
// task stream. What remains here is the fallback each reader takes until its
// scope reports synced, plus the calls that have no streamed counterpart at
// all — the readiness probe, the creation, the deletion, and the six actions.
//
// A predicate over current state reads the same whether the state arrived by
// stream or by poll (design-crd-model.md §7.7), which is what let the readers
// move without any step changing.

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/simplyblock/simplyblock-operator/internal/controllers/controlplane"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// ControlPlane is everything the two reconcilers in this package ask of the
// simplyblock control plane.
type ControlPlane interface {
	// Ready reports whether the control plane can accept a cluster creation.
	// It is the one read with no streamed counterpart, because it is asked
	// once per creation rather than watched.
	Ready(ctx context.Context) error

	// CreateCluster is not idempotent, which is why the creation machine
	// claims its slot in Kubernetes before calling it.
	CreateCluster(ctx context.Context, params utils.ClusterAddParams) (webapi.ClusterResponse, error)

	// Cluster reads one cluster by its UUID.
	Cluster(ctx context.Context, clusterID string) (webapi.ClusterResponse, error)

	// ClusterByName finds a cluster the operator may not have created. It
	// reports false rather than an error when there is none, because "no such
	// cluster" is the ordinary answer on the creation path.
	ClusterByName(ctx context.Context, name string) (utils.ClusterListEntry, bool, error)

	// DeleteCluster is retried until it succeeds, and the finalizer is not
	// removed before it does.
	DeleteCluster(ctx context.Context, clusterID string) error

	// The four cluster-level actions. Each must tolerate a repeat, because a
	// step recorded without its call having fired re-issues it.
	Activate(ctx context.Context, clusterID string) error
	Expand(ctx context.Context, clusterID string) error
	Shutdown(ctx context.Context, clusterID string) error
	Start(ctx context.Context, clusterID string) error

	// StorageNodes are the cluster's nodes as the control plane reports them,
	// which is what the rolling restart's every predicate is evaluated
	// against.
	StorageNodes(ctx context.Context, clusterID string) ([]utils.NodeStatusResponse, error)
	ShutdownNode(ctx context.Context, clusterID, nodeID string) error
	RestartNode(ctx context.Context, clusterID, nodeID string) error

	// Tasks are the control plane's own asynchronous jobs, which
	// StorageCluster.status.tasks is a window on. It is the fallback the task
	// stream's cache is preferred over.
	Tasks(ctx context.Context, clusterID string) ([]subscriptions.TaskDTO, error)

	// CancelTask asks for one to stop. A task that is already gone is the
	// outcome being asked for reached by another route, so the implementation
	// reads a 404 as success.
	CancelTask(ctx context.Context, clusterID, taskID string) error

	// Endpoint is the base URL this call would currently reach the control
	// plane at, resolved the same way every other call resolves it (§3.3).
	// upsertCSICredentials carries it into the CSI driver's aggregate Secret,
	// since the CSI driver dials it directly rather than through this
	// reconciler, and a hardcoded in-cluster address is wrong the moment the
	// control plane is a ControlPlane.spec.source.managed one.
	Endpoint(ctx context.Context) string
}

// httpControlPlane is the ControlPlane the operator runs with: one method per
// endpoint of §9, over a client whose address comes from the ControlPlane object.
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

func (c *httpControlPlane) Endpoint(ctx context.Context) string { return c.clientFor(ctx).BaseURL }

func (c *httpControlPlane) Ready(ctx context.Context) error {
	_, err := c.call(ctx, http.MethodGet, "/api/v2/_meta/ready", nil)
	return err
}

func (c *httpControlPlane) CreateCluster(
	ctx context.Context, params utils.ClusterAddParams,
) (webapi.ClusterResponse, error) {
	body, err := c.call(ctx, http.MethodPost, "/api/v2/clusters/", params)
	if err != nil {
		return webapi.ClusterResponse{}, err
	}
	return webapi.ParseClusterResponse(body)
}

func (c *httpControlPlane) Cluster(
	ctx context.Context, clusterID string,
) (webapi.ClusterResponse, error) {
	body, err := c.call(ctx, http.MethodGet, "/api/v2/clusters/"+clusterID, nil)
	if err != nil {
		return webapi.ClusterResponse{}, err
	}
	return webapi.ParseClusterResponse(body)
}

func (c *httpControlPlane) ClusterByName(
	ctx context.Context, name string,
) (utils.ClusterListEntry, bool, error) {
	body, err := c.call(ctx, http.MethodGet, "/api/v2/clusters/", nil)
	if err != nil {
		return utils.ClusterListEntry{}, false, err
	}
	var entries []utils.ClusterListEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return utils.ClusterListEntry{}, false, fmt.Errorf("read the cluster list: %w", err)
	}
	for _, entry := range entries {
		if entry.Name == name {
			return entry, true, nil
		}
	}
	return utils.ClusterListEntry{}, false, nil
}

func (c *httpControlPlane) DeleteCluster(ctx context.Context, clusterID string) error {
	_, err := c.call(ctx, http.MethodDelete, "/api/v2/clusters/"+clusterID, nil)
	return err
}

func (c *httpControlPlane) Activate(ctx context.Context, clusterID string) error {
	return c.post(ctx, fmt.Sprintf("/api/v2/clusters/%s/activate", clusterID), nil)
}

func (c *httpControlPlane) Expand(ctx context.Context, clusterID string) error {
	return c.post(ctx, fmt.Sprintf("/api/v2/clusters/%s/expand", clusterID), nil)
}

func (c *httpControlPlane) Shutdown(ctx context.Context, clusterID string) error {
	return c.post(ctx, fmt.Sprintf("/api/v2/clusters/%s/shutdown", clusterID), nil)
}

func (c *httpControlPlane) Start(ctx context.Context, clusterID string) error {
	return c.post(ctx, fmt.Sprintf("/api/v2/clusters/%s/start", clusterID), nil)
}

func (c *httpControlPlane) StorageNodes(
	ctx context.Context, clusterID string,
) ([]utils.NodeStatusResponse, error) {
	body, err := c.call(ctx, http.MethodGet,
		fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterID), nil)
	if err != nil {
		return nil, err
	}
	var nodes []utils.NodeStatusResponse
	if err := json.Unmarshal(body, &nodes); err != nil {
		return nil, fmt.Errorf("read the storage node list: %w", err)
	}
	return nodes, nil
}

func (c *httpControlPlane) ShutdownNode(ctx context.Context, clusterID, nodeID string) error {
	return c.post(ctx,
		fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/shutdown", clusterID, nodeID), nil)
}

// RestartNode forces the restart, which is what the walk means by it: the node
// was shut down by the step before, and a restart that declined because the
// node is not running would leave the walk holding forever.
func (c *httpControlPlane) RestartNode(ctx context.Context, clusterID, nodeID string) error {
	return c.post(ctx,
		fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/restart", clusterID, nodeID),
		map[string]bool{"force": true})
}

func (c *httpControlPlane) Tasks(
	ctx context.Context, clusterID string,
) ([]subscriptions.TaskDTO, error) {
	body, err := c.call(ctx, http.MethodGet,
		fmt.Sprintf("/api/v2/clusters/%s/tasks/", clusterID), nil)
	if err != nil {
		return nil, err
	}
	var tasks []subscriptions.TaskDTO
	if err := json.Unmarshal(body, &tasks); err != nil {
		return nil, fmt.Errorf("read the task list: %w", err)
	}
	return tasks, nil
}

// CancelTask reads a 404 as success. The task finished on its own between the
// operation being written and its Requesting step, which is the outcome the
// operation asked for reached by another route (§6.3).
//
// The endpoint is not provided by the v2 API yet, which is recorded in §9 as
// the one prerequisite the CancelTask action cannot ship without. Until it is,
// this call reports what the control plane reports, and the operation fails
// with the refusal in its message rather than pretending to have canceled
// anything.
func (c *httpControlPlane) CancelTask(ctx context.Context, clusterID, taskID string) error {
	err := c.post(ctx,
		fmt.Sprintf("/api/v2/clusters/%s/tasks/%s/cancel", clusterID, taskID), nil)
	var refusal *ControlPlaneError
	if errors.As(err, &refusal) && refusal.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// post issues a call whose body is not read, which every action endpoint is.
func (c *httpControlPlane) post(ctx context.Context, path string, body any) error {
	_, err := c.call(ctx, http.MethodPost, path, body)
	return err
}

// call performs one request and turns a non-2xx into a ControlPlaneError
// carrying the status and the body, which is what makes a refusal visible in
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

// ControlPlaneError is a request the control plane refused. It carries the
// status and the whole body, because the body is where the control plane says
// why.
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
// published one, which is what makes a remote control plane reachable: the
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
