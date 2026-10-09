// The control plane's backup layer: the copies a cluster's store holds, the
// policies that schedule them, and the restore that turns one back into a
// volume.
//
// It lives here rather than in the operator for the reason every other file in
// this package does: a backup is a control-plane object, and a second
// implementation of these calls in a consumer would drift from this one. The
// operator is the only consumer today, and what decides where the code lives is
// what it is about rather than who happens to need it.

package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/ptr"
)

// backupPolicyIDHeader is where the create endpoint returns the identifier of
// the policy it made. The response body is untyped in the spec, so the header is
// the only place the id appears.
const backupPolicyIDHeader = "X-Policy-Id"

// AttachTargetVolume is the target type a backup policy is attached to. The
// control plane's attach endpoint takes a type and an id so that it can grow
// other targets; a volume is the only one this product uses.
const AttachTargetVolume = "lvol"

// Backup is one copy of one volume in a cluster's store.
//
// There is no call here that deletes one, and that is the design rather than an
// omission. A copy is governed by its bucket's lifecycle policy and by the
// retention the control plane applies, and nothing in this product prunes on its
// behalf (design-storagebackup.md §9). The endpoint that would, which takes a
// volume rather than a backup because the control plane holds a volume's copies
// as one incremental chain, is deprecated upstream as well.
//
// The cluster that wrote the copy is not among the fields, because the wire no
// longer carries it. A store several clusters have configured reports every
// backup in it to all of them, so the reporting cluster is the scope of the call
// rather than a property of the backup, and a caller that needs the writer reads
// the exported manifest instead.
//
// Times are absolute instants rather than the Unix seconds the wire carries, and
// a zero Time means the control plane reported none: a backup that has not
// completed has no completion instant, and 1970 is not the honest way to say so.
type Backup struct {
	ID           string
	S3ID         int64
	LvolID       string
	LvolName     string
	SnapshotID   string
	SnapshotName string
	NodeID       string
	Status       string
	PrevBackupID string
	Encrypted    bool
	SizeBytes    int64
	CreatedAt    time.Time
	CompletedAt  time.Time
}

func backupFromDTO(d cpapi.BackupDTO) Backup {
	return Backup{
		ID:           d.Id.String(),
		S3ID:         int64(d.S3Id),
		LvolID:       d.LvolId.String(),
		LvolName:     d.LvolName,
		SnapshotID:   d.SnapshotId.String(),
		SnapshotName: d.SnapshotName,
		NodeID:       d.NodeId.String(),
		Status:       d.Status,
		PrevBackupID: uuidOrEmpty(d.PrevBackupId),
		Encrypted:    d.Encrypted,
		SizeBytes:    int64(d.Size),
		CreatedAt:    unixSeconds(d.CreatedAt),
		CompletedAt:  unixSeconds(d.CompletedAt),
	}
}

// uuidOrEmpty renders an optional identifier, reading an absent one as the empty
// string. The nil UUID is not the honest way to say that a field was not sent:
// the first backup of a chain has no predecessor, and
// "00000000-0000-0000-0000-000000000000" reads as one.
func uuidOrEmpty(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// unixSeconds converts a control-plane timestamp into an instant, reading a
// non-positive value as no timestamp at all.
func unixSeconds(seconds int) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(seconds), 0).UTC()
}

// ListBackups returns every backup in a cluster's store, including the ones
// another cluster wrote: the store is the inventory, and what a cluster can see
// is what its configured location holds.
func (c *Client) ListBackups(ctx context.Context, clusterID string) ([]Backup, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.ClustersBackupsListApiV2ClustersClusterIdBackupsGetWithResponse(ctx, cluster, nil)
	if err != nil {
		return nil, fmt.Errorf("list backups in %s: %w", clusterID, err)
	}
	ds, err := payload("list backups in "+clusterID, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]Backup, 0, len(*ds))
	for _, d := range *ds {
		out = append(out, backupFromDTO(d))
	}
	return out, nil
}

// BackupByID returns one backup from a cluster's store. The v2 list endpoint is
// the only one that reports every field, so this filters it rather than asking
// for the backup by id. It wraps errs.ErrNotFound when nothing matches.
func (c *Client) BackupByID(ctx context.Context, clusterID, backupID string) (Backup, error) {
	backups, err := c.ListBackups(ctx, clusterID)
	if err != nil {
		return Backup{}, err
	}
	for _, backup := range backups {
		if backup.ID == backupID {
			return backup, nil
		}
	}
	return Backup{}, fmt.Errorf("backup %q in cluster %s: %w", backupID, clusterID, errs.ErrNotFound)
}

// BackupPolicy is a schedule and a retention the control plane applies to the
// volumes attached to it.
type BackupPolicy struct {
	ID          string
	Name        string
	Schedule    string
	MaxAge      string
	MaxVersions int32
	Status      string
}

func backupPolicyFromDTO(d cpapi.BackupPolicyDTO) BackupPolicy {
	return BackupPolicy{
		ID:          d.Id.String(),
		Name:        d.Name,
		Schedule:    d.BackupSchedule,
		MaxAge:      d.MaxAge,
		MaxVersions: int32(d.MaxVersions),
		Status:      d.Status,
	}
}

// ListBackupPolicies returns every backup policy in a cluster.
func (c *Client) ListBackupPolicies(ctx context.Context, clusterID string) ([]BackupPolicy, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.ClustersBackupPoliciesListApiV2ClustersClusterIdBackupsBackupPoliciesGetWithResponse(ctx, cluster, nil)
	if err != nil {
		return nil, fmt.Errorf("list backup policies in %s: %w", clusterID, err)
	}
	ds, err := payload("list backup policies in "+clusterID, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]BackupPolicy, 0, len(*ds))
	for _, d := range *ds {
		out = append(out, backupPolicyFromDTO(d))
	}
	return out, nil
}

// BackupPolicyByName returns the policy with the given name in a cluster. The
// v2 API has no by-name lookup, so this lists and filters, which is also what
// lets a caller find a policy it created before it recorded the id. It wraps
// errs.ErrNotFound when no policy matches.
func (c *Client) BackupPolicyByName(ctx context.Context, clusterID, name string) (BackupPolicy, error) {
	policies, err := c.ListBackupPolicies(ctx, clusterID)
	if err != nil {
		return BackupPolicy{}, err
	}
	for _, policy := range policies {
		if policy.Name == name {
			return policy, nil
		}
	}
	return BackupPolicy{}, fmt.Errorf("backup policy %q in cluster %s: %w", name, clusterID, errs.ErrNotFound)
}

// CreateBackupPolicyParams are the inputs for creating a backup policy. Only
// Name is required; an empty schedule or retention is a policy the control plane
// applies no limit for.
type CreateBackupPolicyParams struct {
	Name        string
	Schedule    string
	MaxAge      string
	MaxVersions int32
}

// CreateBackupPolicy creates a backup policy and returns its identifier.
//
// The identifier comes from a response header rather than from a body, because
// the endpoint declares no response model. A create that succeeds without one is
// an error rather than an empty id: the caller cannot attach a volume to a
// policy it cannot name, and an empty id would make the next call fail somewhere
// less obvious.
func (c *Client) CreateBackupPolicy(
	ctx context.Context, clusterID string, params CreateBackupPolicyParams,
) (string, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return "", err
	}

	body := cpapi.UnderscorePolicyCreateParams{Name: params.Name}
	if params.Schedule != "" {
		body.Schedule = ptr.To(params.Schedule)
	}
	if params.MaxAge != "" {
		body.Age = ptr.To(params.MaxAge)
	}
	if params.MaxVersions > 0 {
		body.Versions = ptr.To(int(params.MaxVersions))
	}

	what := "create backup policy " + params.Name
	resp, err := c.api.ClustersBackupPoliciesCreateApiV2ClustersClusterIdBackupsBackupPoliciesPostWithResponse(
		ctx, cluster, body)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		return "", respError(what, code, resp.Body)
	}
	if resp.HTTPResponse == nil {
		return "", fmt.Errorf("%s: the response carried no headers: %w", what, errs.ErrInvalidResponse)
	}
	policyID := resp.HTTPResponse.Header.Get(backupPolicyIDHeader)
	if policyID == "" {
		return "", fmt.Errorf("%s: the response carried no %s header: %w",
			what, backupPolicyIDHeader, errs.ErrInvalidResponse)
	}
	return policyID, nil
}

// DeleteBackupPolicy removes a policy. A policy that is already gone is a
// success, so that a caller retrying a delete converges rather than failing on
// the second attempt.
func (c *Client) DeleteBackupPolicy(ctx context.Context, clusterID, policyID string) error {
	cluster, policy, err := parseIDs(clusterID, policyID)
	if err != nil {
		return err
	}
	what := "delete backup policy " + policyID
	resp, err := c.api.ClustersBackupPoliciesDeleteApiV2ClustersClusterIdBackupsBackupPoliciesPolicyIdDeleteWithResponse(
		ctx, cluster, policy)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return okOrGone(what, resp.StatusCode(), resp.Body)
}

// AttachBackupPolicy puts a volume under a policy, so that the control plane
// starts taking the copies the policy schedules.
func (c *Client) AttachBackupPolicy(ctx context.Context, clusterID, policyID, volumeID string) error {
	cluster, policy, err := parseIDs(clusterID, policyID)
	if err != nil {
		return err
	}
	what := fmt.Sprintf("attach backup policy %s to volume %s", policyID, volumeID)
	resp, err := c.api.ClustersBackupPoliciesAttachApiV2ClustersClusterIdBackupsBackupPoliciesPolicyIdAttachPostWithResponse(
		ctx, cluster, policy, cpapi.UnderscoreAttachParams{TargetType: AttachTargetVolume, TargetId: volumeID})
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		return respError(what, code, resp.Body)
	}
	return nil
}

// DetachBackupPolicy takes a volume back out from under a policy. It stops new
// copies being taken and deletes none of the existing ones.
//
// An attachment that is already gone is a success. The control plane reports
// that case as a 400 naming the missing attachment rather than as a 404, which
// is why the check below reads the body: a detach has to be idempotent, because
// the caller retries it and because a claim can stop matching a selector twice.
func (c *Client) DetachBackupPolicy(ctx context.Context, clusterID, policyID, volumeID string) error {
	cluster, policy, err := parseIDs(clusterID, policyID)
	if err != nil {
		return err
	}
	what := fmt.Sprintf("detach backup policy %s from volume %s", policyID, volumeID)
	resp, err := c.api.ClustersBackupPoliciesDetachApiV2ClustersClusterIdBackupsBackupPoliciesPolicyIdDetachPostWithResponse(
		ctx, cluster, policy, cpapi.UnderscoreAttachParams{TargetType: AttachTargetVolume, TargetId: volumeID})
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	code := resp.StatusCode()
	if code >= 200 && code < 300 {
		return nil
	}
	if attachmentAlreadyGone(code, resp.Body) {
		return nil
	}
	return respError(what, code, resp.Body)
}

// RestoreBackupParams are the inputs for restoring one backup into a new volume.
type RestoreBackupParams struct {
	BackupID string
	LvolName string
	// Pool is the pool to restore into, by name.
	Pool string
	// TargetNodeID places the restored volume on one storage node. Empty lets
	// the control plane choose.
	TargetNodeID string
}

// RestoreBackup asks the control plane to read one backup out of the store into
// a new logical volume, and returns that volume's identifier.
//
// The call returns as soon as the restore is accepted; the transfer runs
// afterward and the volume reports its own progress. The endpoint declares no
// response model, so the body is decoded here.
func (c *Client) RestoreBackup(
	ctx context.Context, clusterID string, params RestoreBackupParams,
) (string, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return "", err
	}

	body := cpapi.UnderscoreRestoreParams{
		BackupId: params.BackupID,
		LvolName: params.LvolName,
		Pool:     params.Pool,
	}
	if params.TargetNodeID != "" {
		body.TargetNodeId = ptr.To(params.TargetNodeID)
	}

	what := "restore backup " + params.BackupID
	resp, err := c.api.ClustersBackupsRestoreApiV2ClustersClusterIdBackupsRestorePostWithResponse(ctx, cluster, body)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	restored, err := decodeBody[struct {
		LvolID string `json:"lvol_id"`
	}](what, resp.StatusCode(), resp.Body)
	if err != nil {
		return "", err
	}
	if restored.LvolID == "" {
		return "", fmt.Errorf("%s: the response named no restored volume: %w", what, errs.ErrInvalidResponse)
	}
	return restored.LvolID, nil
}

// backupIDHeader carries the new backup's identifier on a successful create,
// which declares no response model.
const backupIDHeader = "X-Backup-Id"

// CreateBackup asks the control plane to back one snapshot up into the
// cluster's store, and returns the backup's identifier.
//
// The call returns once the backup is accepted. The transfer runs afterward,
// and BackupByID reports how far it has got. A refusal, such as a cluster with
// no backup store configured, answers 400 with the reason, and the reason is
// kept in the returned error.
func (c *Client) CreateBackup(ctx context.Context, clusterID, snapshotID string) (string, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return "", err
	}

	what := "back up snapshot " + snapshotID
	resp, err := c.api.ClustersBackupsCreateApiV2ClustersClusterIdBackupsPostWithResponse(
		ctx, cluster, nil, cpapi.UnderscoreBackupSnapshotParams{SnapshotId: snapshotID})
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		return "", respError(what, code, resp.Body)
	}
	if resp.HTTPResponse == nil {
		return "", fmt.Errorf("%s: the response carried no headers: %w", what, errs.ErrInvalidResponse)
	}
	backupID := resp.HTTPResponse.Header.Get(backupIDHeader)
	if backupID == "" {
		return "", fmt.Errorf("%s: the response carried no %s header: %w",
			what, backupIDHeader, errs.ErrInvalidResponse)
	}
	return backupID, nil
}

// okOrGone accepts a success and a 404 alike, which is what makes a delete
// idempotent: the caller asked for the resource to be absent, and it is.
func okOrGone(what string, code int, body []byte) error {
	if (code >= 200 && code < 300) || code == http.StatusNotFound {
		return nil
	}
	return respError(what, code, body)
}

// attachmentAlreadyGone reports the response the control plane gives for
// detaching something that is not attached. It answers 400 with a message rather
// than 404, so both halves are matched: the status alone would swallow every
// other bad request, and the message alone would match a 500 that happened to
// quote it. The match is case-insensitive so that a rewording of the surrounding
// text does not silently turn a converged detach back into a failure.
func attachmentAlreadyGone(code int, body []byte) bool {
	return code == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(string(body)), "attachment not found")
}
