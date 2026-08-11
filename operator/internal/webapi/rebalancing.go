package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// StorageNodeInfo holds fields from GET /api/v2/clusters/{id}/storage-nodes/.
type StorageNodeInfo struct {
	UUID       string `json:"id"`
	Status     string `json:"status"`
	Healthy    bool   `json:"health_check"`
	TotalBytes int64  `json:"total_capacity_bytes"`
	// Lvols and LvolsMax are the current and maximum lvol-subsystem counts for the
	// node, used to filter out at-capacity nodes during primary node placement.
	Lvols    int `json:"lvols"`
	LvolsMax int `json:"lvols_max"`
}

// CapacityStat holds the capacity sub-object present on VolumeDTO.
type CapacityStat struct {
	SizeUsed int64 `json:"size_used"`
}

// VolumeInfo holds fields from VolumeDTO returned by
// GET /api/v2/clusters/{id}/storage-pools/{id}/volumes/.
type VolumeInfo struct {
	UUID string `json:"id"`
	Name string `json:"name"`
	// NQN is the volume's NVMe subsystem NQN. Namespaced volumes share it with
	// their siblings, and it is the identity a batch migration is addressed by
	// (see MigrationRef).
	NQN                   string       `json:"nqn"`
	PrimaryNodeUUID       string       `json:"storage_node_id"`
	Status                string       `json:"status"`
	Migrating             bool         `json:"migrating"`
	Capacity              CapacityStat `json:"capacity"`
	IOPS                  float64      `json:"iops"`
	ThroughputBytesPerSec float64      `json:"throughput_bytes_per_sec"`
}

// StoragePoolInfo holds the fields needed from GET /api/v2/clusters/{id}/storage-pools/.
type StoragePoolInfo struct {
	UUID string `json:"id"`
	Name string `json:"name"`
}

// ContinueMigrationParams is the request body for the continue migration endpoint.
// MigrationID is identified via the URL path; this body carries optional tuning params only.
type ContinueMigrationParams struct {
	MaxRetries      int `json:"max_retries,omitempty"`
	DeadlineSeconds int `json:"deadline_seconds,omitempty"`
}

// Migrations are addressed by cluster and NVMe subsystem NQN: the control plane
// migrates a whole subsystem at once, covering both a single-namespace subsystem
// and a namespaced one, where several volumes share it.
//
// migrationsURL is the list/create endpoint for a subsystem's migrations,
// migrationURL the detail/cancel endpoint of a single migration. Both carry the
// trailing slash the control plane declares them with; without it every call
// costs a 307 redirect first.
func migrationsURL(clusterUUID, nqn string) string {
	return fmt.Sprintf("/api/v2/clusters/%s/subsystems/%s/migrations/",
		clusterUUID, url.PathEscape(nqn))
}

func migrationURL(clusterUUID, nqn, migrationID string) string {
	return fmt.Sprintf("%s%s/", migrationsURL(clusterUUID, nqn), migrationID)
}

// LvolConnectResp holds the NVMe-oF connection parameters for a logical volume,
// as returned by CreateMigration for the new target-side paths that must be
// connected and validated before calling ContinueMigration.
type LvolConnectResp struct {
	Nqn            string `json:"nqn"`
	ReconnectDelay int    `json:"reconnect-delay"`
	NrIoQueues     int    `json:"nr-io-queues"`
	CtrlLossTmo    int    `json:"ctrl-loss-tmo"`
	FastIOFailTmo  int    `json:"fast-io-fail-tmo"`
	KeepAliveTmo   int    `json:"keep-alive-tmo"`
	Port           int    `json:"port"`
	TargetType     string `json:"transport"`
	IP             string `json:"ip"`
	Connect        string `json:"connect"`
	NSID           int    `json:"ns-id"`
	HostIface      string `json:"host-iface,omitempty"`
}

// MigrateParams is the request body for
// POST /api/v2/clusters/{id}/subsystems/{nqn}/migrations.
type MigrateParams struct {
	TargetNodeID string `json:"target_node_id"`
}

// MigrationDTO is returned by POST (create), GET (poll), and ContinueMigration.
// It describes the migration of one whole NVMe subsystem: TargetNQN identifies
// the subsystem and MemberCount how many volumes (namespaces) move with it.
//
// The subsystem endpoints return one of two shapes, depending on how the volume
// was provisioned: a batch migration of a shared subsystem carries target_nqn
// and member_count, a migration of a single-namespace subsystem carries neither
// (plus snapshot and retry counters this client does not read). normalize()
// reconciles the difference so callers see one shape.
type MigrationDTO struct {
	ID             string            `json:"id"`
	ClusterID      string            `json:"cluster_id"`
	SourceNodeID   string            `json:"source_node_id"`
	TargetNodeID   string            `json:"target_node_id"`
	TargetNQN      string            `json:"target_nqn"`
	Phase          string            `json:"phase"`
	Status         string            `json:"status"`
	MemberCount    int               `json:"member_count"`
	ErrorMessage   string            `json:"error_message"`
	ConnectStrings []LvolConnectResp `json:"connect_strings"`
}

// normalize fills in what a single-namespace migration's response leaves out.
// Such a migration still moves exactly one volume, so reporting 0 members would
// make "how many volumes did this move" wrong for every non-namespaced volume;
// and it is addressed under the subsystem the caller asked for, so that NQN is
// the subsystem being migrated whether or not the response repeats it.
func (m *MigrationDTO) normalize(nqn string) {
	if m.MemberCount <= 0 {
		m.MemberCount = 1
	}
	if m.TargetNQN == "" {
		m.TargetNQN = nqn
	}
}

// Migration status values reported in MigrationDTO.Status. The status field —
// not error_message — is the authoritative signal for whether a migration has
// finished and whether it succeeded: a transient error_message may linger from
// a retried-then-recovered step even when the migration ultimately completes.
const (
	// MigrationStatusNew, MigrationStatusRunning, MigrationStatusSuspended and
	// MigrationStatusCutover are non-terminal: the migration is still in flight.
	MigrationStatusNew       = "new"
	MigrationStatusRunning   = "running"
	MigrationStatusSuspended = "suspended"
	MigrationStatusCutover   = "cutover"

	// MigrationStatusDone, MigrationStatusFailed and MigrationStatusCancelled are
	// the terminal states. Only MigrationStatusDone is a success.
	MigrationStatusDone      = "done"
	MigrationStatusFailed    = "failed"
	MigrationStatusCancelled = "cancelled"
)

// Migration phase values reported in MigrationDTO.Phase. The backend advances a
// migration through these phases; only MigrationPhasePreCreated accepts a
// ContinueMigration call. Once past pre_created, ContinueMigration is rejected,
// so callers use the phase to make the continue step idempotent across retries.
const (
	// MigrationPhasePreCreated is the initial phase after CreateMigration: the
	// target infrastructure exists but the data migration has not been started.
	// ContinueMigration is only valid in this phase. Both a single-namespace and a
	// shared-subsystem migration report it under this one name.
	MigrationPhasePreCreated = "pre_created"
)

// MigrationIsTerminal reports whether a migration status is terminal
// (done, failed, or cancelled) and therefore no longer in flight.
func MigrationIsTerminal(status string) bool {
	switch status {
	case MigrationStatusDone, MigrationStatusFailed, MigrationStatusCancelled:
		return true
	default:
		return false
	}
}

// GetStoragePools lists all storage pools for the given cluster.
func (c *Client) GetStoragePools(
	ctx context.Context,
	clusterUUID string,
) ([]StoragePoolInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("list storage pools: %w", err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("list storage pools: status %d: %s", statusCode, string(body))
	}
	var pools []StoragePoolInfo
	if err := json.Unmarshal(body, &pools); err != nil {
		return nil, fmt.Errorf("unmarshal storage pools: %w", err)
	}
	return pools, nil
}

// GetStorageNodes lists all storage nodes for the given cluster.
func (c *Client) GetStorageNodes(
	ctx context.Context,
	clusterUUID string,
) ([]StorageNodeInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("list storage nodes: %w", err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("list storage nodes: status %d: %s", statusCode, string(body))
	}
	var nodes []StorageNodeInfo
	if err := json.Unmarshal(body, &nodes); err != nil {
		return nil, fmt.Errorf("unmarshal storage nodes: %w", err)
	}
	return nodes, nil
}

// GetPoolVolumes lists all volumes in the given storage pool.
func (c *Client) GetPoolVolumes(
	ctx context.Context,
	clusterUUID, poolUUID string,
) ([]VolumeInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/", clusterUUID, poolUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("list pool volumes: %w", err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("list pool volumes: status %d: %s", statusCode, string(body))
	}
	var volumes []VolumeInfo
	if err := json.Unmarshal(body, &volumes); err != nil {
		return nil, fmt.Errorf("unmarshal pool volumes: %w", err)
	}
	return volumes, nil
}

// GetSubsystemVolumes returns every volume in the cluster that shares the NVMe
// subsystem nqn — the set a migration of that subsystem moves as a unit. For a
// single-namespace subsystem that is one volume; for a namespaced one it is the
// volume and its siblings.
//
// The control plane has no volume-by-NQN lookup and its batch-migration DTO reports
// only a member *count*, so membership is derived here by scanning the cluster's
// pools. Pools are scanned rather than assuming the subsystem's members live in the
// pool of any one member: a subsystem is scoped to a storage node, not to a pool.
func (c *Client) GetSubsystemVolumes(
	ctx context.Context,
	clusterUUID, nqn string,
) ([]VolumeInfo, error) {
	if nqn == "" {
		return nil, fmt.Errorf("list volumes of subsystem: empty NQN")
	}
	pools, err := c.GetStoragePools(ctx, clusterUUID)
	if err != nil {
		return nil, fmt.Errorf("list volumes of subsystem %s: %w", nqn, err)
	}
	var members []VolumeInfo
	for _, p := range pools {
		vols, err := c.GetPoolVolumes(ctx, clusterUUID, p.UUID)
		if err != nil {
			return nil, fmt.Errorf("list volumes of subsystem %s: pool %s: %w", nqn, p.UUID, err)
		}
		for _, v := range vols {
			if v.NQN == nqn {
				members = append(members, v)
			}
		}
	}
	return members, nil
}

// GetVolume fetches a single volume by its cluster/pool/volume UUIDs (all known
// from the CSI volume handle). Returns (nil, nil) when the volume no longer exists.
func (c *Client) GetVolume(
	ctx context.Context,
	clusterUUID, poolUUID, volumeUUID string,
) (*VolumeInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/", clusterUUID, poolUUID, volumeUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("get volume %s: %w", volumeUUID, err)
	}
	if statusCode == http.StatusNotFound {
		return nil, nil
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("get volume %s: status %d: %s", volumeUUID, statusCode, string(body))
	}
	// The endpoint may return a single object or a one-element array depending on
	// the backend version; accept both.
	var one VolumeInfo
	if err := json.Unmarshal(body, &one); err == nil && one.UUID != "" {
		return &one, nil
	}
	var list []VolumeInfo
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("unmarshal volume %s: %w", volumeUUID, err)
	}
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

// StorageNodeNIC is one network interface entry returned by the storage-node
// /nics endpoint. Address is the data-network IP the lvol subsystem listens on
// (the management IP is reported separately). Field tags match the capitalised,
// space-containing keys the control plane emits for this endpoint.
type StorageNodeNIC struct {
	ID         string `json:"ID"`
	DeviceName string `json:"Device name"`
	Address    string `json:"Address"`
	NetType    string `json:"Net type"`
	Status     string `json:"Status"`
}

// GetStorageNodeNICs returns the data-network interfaces for a single storage
// node. Used to target the fio latency baseline at the node's data-NIC IP rather
// than its management IP (the lvol subsystem does not listen on mgmt_ip).
func (c *Client) GetStorageNodeNICs(
	ctx context.Context,
	clusterUUID, nodeUUID string,
) ([]StorageNodeNIC, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/nics", clusterUUID, nodeUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("get NICs for node %s: %w", nodeUUID, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("get NICs for node %s: status %d: %s", nodeUUID, statusCode, string(body))
	}
	var nics []StorageNodeNIC
	if err := json.Unmarshal(body, &nics); err != nil {
		return nil, fmt.Errorf("unmarshal node NICs: %w", err)
	}
	return nics, nil
}

// CreateMigration submits a new migration request for the subsystem identified
// by nqn. Returns a MigrationDTO containing the migration ID and the NVMe-oF
// connection strings for the target-side paths. The caller must establish and
// validate those paths before calling ContinueMigration.
//
// If the API reports that a migration already exists for the subsystem, any
// existing migrations are cancelled and the request is retried once. The API
// signals this as either 409 or 400 with an "...already exists... Cancel it
// first" detail depending on deployment, so both are handled.
func (c *Client) CreateMigration(
	ctx context.Context,
	clusterUUID, nqn, targetNodeID string,
) (*MigrationDTO, error) {
	logger := log.FromContext(ctx)
	params := MigrateParams{TargetNodeID: targetNodeID}

	body, statusCode, err := c.Do(ctx, http.MethodPost, migrationsURL(clusterUUID, nqn), params)
	if err != nil {
		return nil, fmt.Errorf("create migration for subsystem %s: %w", nqn, err)
	}

	if isMigrationNotAcceptingYet(statusCode, body) {
		return nil, fmt.Errorf("create migration for subsystem %s: %s: %w",
			nqn, strings.TrimSpace(string(body)), ErrMigrationNotAcceptingYet)
	}

	if isExistingMigrationConflict(statusCode, body) {
		logger.Info("CreateMigration rejected: a migration already exists for the subsystem; cancelling before retry",
			"subsystem", nqn, "status", statusCode)
		if cancelErr := c.cancelMigrationForSubsystem(ctx, clusterUUID, nqn); cancelErr != nil {
			return nil, fmt.Errorf("create migration for subsystem %s: cancel existing migrations: %w", nqn, cancelErr)
		}
		// After a successful cancellation, the next reconcile cycle should retry to create a migration.
		return nil, nil
	}

	// FIXME: logging the full response body is a debugging aid and should be
	// removed — or at least masked — before this is considered production-ready,
	// since the body may carry NVMe connection details (NQNs, IPs) or other
	// sensitive fields.
	logger.Info("CreateMigration response", "subsystem", nqn, "status", statusCode, "body", string(body))

	if statusCode >= 300 {
		return nil, fmt.Errorf("create migration for subsystem %s: status %d: %s", nqn, statusCode, string(body))
	}
	var m MigrationDTO
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("unmarshal migration response: %w", err)
	}
	m.normalize(nqn)
	logger.Info("CreateMigration parsed", "subsystem", nqn, "migration_id", m.ID,
		"members", m.MemberCount, "connect_strings", len(m.ConnectStrings))
	return &m, nil
}

// ErrMigrationNotAcceptingYet reports that the control plane refused to create the
// migration because of a condition that clears by itself: a cluster-wide data
// rebalance in progress, or a node already busy with a data migration. The request
// is well-formed and the volume is fine — the same call succeeds once the cluster
// settles, so the caller should wait and retry rather than fail the migration.
//
// This matters because the operator itself causes the condition: every completed
// migration flags the cluster for a control-plane data realignment, which the
// rebalancer then triggers, and the control plane rejects new migrations while that
// runs.
var ErrMigrationNotAcceptingYet = errors.New("cluster is not accepting migrations yet")

// isMigrationNotAcceptingYet reports whether a CreateMigration rejection is one of
// those self-clearing conditions. Matched narrowly on the control plane's own
// wording (migration_controller.py's PreconditionError messages) so that a genuinely
// bad request — a volume already on the target node, an unknown node — still fails
// fast instead of being retried forever.
func isMigrationNotAcceptingYet(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	b := strings.ToLower(string(body))
	return strings.Contains(b, "is rebalancing") ||
		strings.Contains(b, "data migration in progress")
}

// isExistingMigrationConflict reports whether a CreateMigration response
// indicates an existing migration is blocking the request. Some deployments
// return 409; others return 400 with a detail like "An active migration for
// <vol> already exists targeting a different node (...). Cancel it first." Only
// that specific 400 is treated as a conflict — other 400s (bad request, volume
// already on the target node, etc.) must not trigger a cancel-and-retry.
func isExistingMigrationConflict(statusCode int, body []byte) bool {
	if statusCode == http.StatusConflict {
		return true
	}
	if statusCode == http.StatusBadRequest {
		b := strings.ToLower(string(body))
		return strings.Contains(b, "already exists") && strings.Contains(b, "migration")
	}
	return false
}

// cancelMigrationForSubsystem cancels the in-flight migration of the subsystem,
// the one that blocks a new request. The list endpoint is already
// subsystem-scoped and returns newest first, so the first non-terminal entry is
// it; already-finished migrations are left alone since cancelling them is not
// valid and would mask the real conflict.
func (c *Client) cancelMigrationForSubsystem(ctx context.Context, clusterUUID, nqn string) error {
	logger := log.FromContext(ctx)
	migrations, err := c.GetMigrations(ctx, clusterUUID, nqn)
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	for _, m := range migrations {
		if MigrationIsTerminal(m.Status) {
			continue
		}
		logger.Info("Cancelling existing migration for subsystem", "migration", m.ID, "subsystem", nqn)
		if err := c.CancelMigration(ctx, clusterUUID, nqn, m.ID); err != nil {
			return fmt.Errorf("cancel migration %s: %w", m.ID, err)
		}
		return nil
	}
	return fmt.Errorf("no in-flight migration found for subsystem %s", nqn)
}

// GetMigrations lists all migrations of the given subsystem, newest first.
func (c *Client) GetMigrations(
	ctx context.Context,
	clusterUUID, nqn string,
) ([]MigrationDTO, error) {
	body, statusCode, err := c.Do(ctx, http.MethodGet, migrationsURL(clusterUUID, nqn), nil)
	if err != nil {
		return nil, fmt.Errorf("list migrations for subsystem %s: %w", nqn, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("list migrations for subsystem %s: status %d: %s", nqn, statusCode, string(body))
	}
	var migrations []MigrationDTO
	if err := json.Unmarshal(body, &migrations); err != nil {
		return nil, fmt.Errorf("unmarshal migrations response: %w", err)
	}
	// The listing mixes both shapes: batch migrations of this subsystem and
	// single-volume ones belonging to it.
	for i := range migrations {
		migrations[i].normalize(nqn)
	}
	return migrations, nil
}

// GetMigration fetches the current status of a migration by its ID.
func (c *Client) GetMigration(
	ctx context.Context,
	clusterUUID, nqn, migrationID string,
) (*MigrationDTO, error) {
	body, statusCode, err := c.Do(ctx, http.MethodGet, migrationURL(clusterUUID, nqn, migrationID), nil)
	if err != nil {
		return nil, fmt.Errorf("get migration %s: %w", migrationID, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("get migration %s: status %d: %s", migrationID, statusCode, string(body))
	}
	var m MigrationDTO
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("unmarshal migration response: %w", err)
	}
	m.normalize(nqn)
	return &m, nil
}

// ContinueMigration kicks off the actual data migration after the caller has
// created and validated the new NVMe-oF connection paths on the target node.
// It must be called after CreateMigration and a successful path validation.
func (c *Client) ContinueMigration(
	ctx context.Context,
	clusterUUID, nqn, migrationID string,
) error {
	// The only migration sub-resource declared without a trailing slash.
	endpoint := migrationURL(clusterUUID, nqn, migrationID) + "continue"
	body, statusCode, err := c.Do(ctx, http.MethodPost, endpoint, ContinueMigrationParams{})
	if err != nil {
		return fmt.Errorf("continue migration %s: %w", migrationID, err)
	}
	if statusCode >= 300 {
		return fmt.Errorf("continue migration %s: status %d: %s", migrationID, statusCode, string(body))
	}
	return nil
}

// CancelMigration cancels an in-progress migration by its ID.
func (c *Client) CancelMigration(
	ctx context.Context,
	clusterUUID, nqn, migrationID string,
) error {
	body, statusCode, err := c.Do(ctx, http.MethodDelete, migrationURL(clusterUUID, nqn, migrationID), nil)
	if err != nil {
		return fmt.Errorf("cancel migration %s: %w", migrationID, err)
	}
	if statusCode >= 300 {
		return fmt.Errorf("cancel migration %s: status %d: %s", migrationID, statusCode, string(body))
	}
	return nil
}
