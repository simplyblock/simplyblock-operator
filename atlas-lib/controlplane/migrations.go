package controlplane

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/simplyblock/atlas/internal/cpapi"
)

// MigrationKind says which of the two shapes a migration has. The control plane
// decides it rather than the caller: a subsystem configured to carry more than
// one namespace migrates as a coordinated batch, and every other one migrates
// on its own.
type MigrationKind string

const (
	// MigrationKindSingle is one volume moving between storage nodes.
	MigrationKindSingle MigrationKind = "Single"
	// MigrationKindBatch is every volume sharing a subsystem moving together,
	// which they must, because they are reached through one NVMe controller and
	// cannot be split across nodes.
	MigrationKindBatch MigrationKind = "Batch"
)

// SubsystemMigration is a migration of what one NVMe subsystem serves, between
// storage nodes. Migrations are addressed by subsystem rather than by volume
// because a batch moves every volume behind one NQN at once, so the subsystem
// is the smallest thing that can be moved.
//
// Kind says which fields carry meaning: LvolID and the retry and snapshot
// counters belong to a single migration, MemberCount and TargetNQN to a batch,
// and everything above them to both.
type SubsystemMigration struct {
	Kind         MigrationKind
	ID           string
	SourceNodeID string
	TargetNodeID string
	Phase        string
	Status       string
	ErrorMessage string

	// Single only.
	LvolID        string
	RetryCount    int
	MaxRetries    int
	SnapsMigrated int
	SnapsTotal    int

	// Batch only.
	MemberCount int
	TargetNQN   string
}

func singleMigrationFromDTO(d cpapi.MigrationDTO) SubsystemMigration {
	return SubsystemMigration{
		Kind:          MigrationKindSingle,
		ID:            d.Id.String(),
		SourceNodeID:  d.SourceNodeId,
		TargetNodeID:  d.TargetNodeId,
		Phase:         d.Phase,
		Status:        d.Status,
		ErrorMessage:  d.ErrorMessage,
		LvolID:        d.LvolId,
		RetryCount:    d.RetryCount,
		MaxRetries:    d.MaxRetries,
		SnapsMigrated: d.SnapsMigrated,
		SnapsTotal:    d.SnapsTotal,
	}
}

func batchMigrationFromDTO(d cpapi.BatchMigrationDTO) SubsystemMigration {
	return SubsystemMigration{
		Kind:         MigrationKindBatch,
		ID:           d.Id.String(),
		SourceNodeID: d.SourceNodeId,
		TargetNodeID: d.TargetNodeId,
		Phase:        d.Phase,
		Status:       d.Status,
		ErrorMessage: d.ErrorMessage,
		MemberCount:  d.MemberCount,
		TargetNQN:    d.TargetNqn,
	}
}

// migrationFromJSON decodes one migration of either shape. The endpoint answers
// an undiscriminated union, so nothing in the envelope says which arrived and
// the two are told apart by the field only one of them has: member_count
// belongs to a batch. Decoding into the wrong type would otherwise succeed and
// silently produce a migration with every distinguishing field zeroed.
func migrationFromJSON(what string, raw []byte) (SubsystemMigration, error) {
	var probe struct {
		MemberCount *int `json:"member_count"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return SubsystemMigration{}, fmt.Errorf("decode %s: %w", what, err)
	}
	if probe.MemberCount != nil {
		var d cpapi.BatchMigrationDTO
		if err := json.Unmarshal(raw, &d); err != nil {
			return SubsystemMigration{}, fmt.Errorf("decode %s: %w", what, err)
		}
		return batchMigrationFromDTO(d), nil
	}
	var d cpapi.MigrationDTO
	if err := json.Unmarshal(raw, &d); err != nil {
		return SubsystemMigration{}, fmt.Errorf("decode %s: %w", what, err)
	}
	return singleMigrationFromDTO(d), nil
}

// ListSubsystemMigrations returns the migrations of the subsystem nqn, of both
// kinds.
func (c *Client) ListSubsystemMigrations(ctx context.Context, clusterID, nqn string) ([]SubsystemMigration, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.ClustersSubsystemsMigrationsListApiV2ClustersClusterIdSubsystemsNqnMigrationsGetWithResponse(ctx, cluster, nqn)
	if err != nil {
		return nil, fmt.Errorf("list migrations for subsystem %s: %w", nqn, err)
	}
	what := "migrations for subsystem " + nqn
	items, err := payload(what, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]SubsystemMigration, 0, len(*items))
	for _, item := range *items {
		raw, err := item.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", what, err)
		}
		m, err := migrationFromJSON(what, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// GetSubsystemMigration returns a single migration of the subsystem nqn. It
// wraps errs.ErrNotFound when the migration does not exist.
func (c *Client) GetSubsystemMigration(ctx context.Context, clusterID, nqn, migrationID string) (SubsystemMigration, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return SubsystemMigration{}, err
	}
	migration, err := parseUUID("migration id", migrationID)
	if err != nil {
		return SubsystemMigration{}, err
	}
	resp, err := c.api.ClustersSubsystemsMigrationsDetailApiV2ClustersClusterIdSubsystemsNqnMigrationsMigrationIdGetWithResponse(ctx, cluster, nqn, migration)
	if err != nil {
		return SubsystemMigration{}, fmt.Errorf("get migration %s: %w", migrationID, err)
	}
	what := "migration " + migrationID
	body, err := payload(what, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return SubsystemMigration{}, err
	}
	raw, err := body.MarshalJSON()
	if err != nil {
		return SubsystemMigration{}, fmt.Errorf("decode %s: %w", what, err)
	}
	return migrationFromJSON(what, raw)
}

// CreateSubsystemMigration starts moving what the subsystem nqn serves to the
// target storage node and returns the created migration. Whether that is one
// volume or a coordinated batch is the control plane's decision, taken from the
// subsystem's own namespace capacity, and it is reported by the returned Kind.
func (c *Client) CreateSubsystemMigration(ctx context.Context, clusterID, nqn, targetNodeID string) (SubsystemMigration, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return SubsystemMigration{}, err
	}
	target, err := parseUUID("target node id", targetNodeID)
	if err != nil {
		return SubsystemMigration{}, err
	}
	body := cpapi.UnderscoreMigrationParams{TargetNodeId: target}
	resp, err := c.api.ClustersSubsystemsMigrationsCreateApiV2ClustersClusterIdSubsystemsNqnMigrationsPostWithResponse(ctx, cluster, nqn, nil, body)
	if err != nil {
		return SubsystemMigration{}, fmt.Errorf("create migration for subsystem %s: %w", nqn, err)
	}
	what := "create migration for subsystem " + nqn
	// The spec declares the created response as having no content while the
	// endpoint answers the full migration, so the body is decoded here rather
	// than through a generated field.
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		return SubsystemMigration{}, respError(what, code, resp.Body)
	}
	return migrationFromJSON(what, resp.Body)
}

// ContinueSubsystemMigration resumes a paused (for example, pre-created)
// migration of the subsystem nqn.
func (c *Client) ContinueSubsystemMigration(ctx context.Context, clusterID, nqn, migrationID string) error {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return err
	}
	migration, err := parseUUID("migration id", migrationID)
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersSubsystemsMigrationsContinueApiV2ClustersClusterIdSubsystemsNqnMigrationsMigrationIdContinuePostWithResponse(
		ctx, cluster, nqn, migration, cpapi.UnderscoreContinueParams{})
	if err != nil {
		return fmt.Errorf("continue migration %s: %w", migrationID, err)
	}
	return migrationActionResult("continue migration "+migrationID, resp.StatusCode(), resp.Body)
}

// CancelSubsystemMigration cancels a migration of the subsystem nqn.
func (c *Client) CancelSubsystemMigration(ctx context.Context, clusterID, nqn, migrationID string) error {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return err
	}
	migration, err := parseUUID("migration id", migrationID)
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersSubsystemsMigrationsCancelApiV2ClustersClusterIdSubsystemsNqnMigrationsMigrationIdDeleteWithResponse(ctx, cluster, nqn, migration)
	if err != nil {
		return fmt.Errorf("cancel migration %s: %w", migrationID, err)
	}
	return migrationActionResult("cancel migration "+migrationID, resp.StatusCode(), resp.Body)
}

// CleanupSubsystemMigrationTarget releases what a migration left on its target
// node. It is the recovery path for a migration that failed after the target
// was populated, where the volumes still serve from the source and the target's
// copy is otherwise orphaned.
func (c *Client) CleanupSubsystemMigrationTarget(ctx context.Context, clusterID, nqn, migrationID string) error {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return err
	}
	migration, err := parseUUID("migration id", migrationID)
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersSubsystemsMigrationsCleanupTargetApiV2ClustersClusterIdSubsystemsNqnMigrationsMigrationIdCleanupTargetPostWithResponse(ctx, cluster, nqn, migration)
	if err != nil {
		return fmt.Errorf("clean up migration %s target: %w", migrationID, err)
	}
	return migrationActionResult("clean up migration "+migrationID+" target", resp.StatusCode(), resp.Body)
}

// migrationActionResult treats any 2xx as success for the fire-and-forget
// cancel, continue, and cleanup actions (their bodies are untyped in the spec).
func migrationActionResult(what string, code int, body []byte) error {
	if code >= 200 && code < 300 {
		return nil
	}
	return respError(what, code, body)
}
