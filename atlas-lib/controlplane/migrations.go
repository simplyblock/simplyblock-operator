package controlplane

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/simplyblock/atlas/internal/cpapi"
)

// MigrationKind is what one migration moves.
//
// The control plane decides it rather than the caller: a subsystem configured
// for several namespaces migrates as one coordinated group, and one configured
// for a single namespace migrates that volume. A request therefore names a
// target node and nothing about the shape, and the answer says which was made.
type MigrationKind string

const (
	// MigrationOfVolume moves one volume, and reports its snapshot progress.
	MigrationOfVolume MigrationKind = "volume"

	// MigrationOfSubsystem moves every namespace of one subsystem together, and
	// reports how many members that is rather than any one volume's progress.
	MigrationOfSubsystem MigrationKind = "subsystem"
)

// Migration is a migration between storage nodes, of either kind.
//
// The two shapes are one type because they arrive from one endpoint, in one
// list, under one id, and a caller that asked for a migration by id cannot know
// in advance which came back. Kind says which, and the fields it does not apply
// to are the zero value.
type Migration struct {
	Kind MigrationKind

	ID           string
	SourceNodeID string
	TargetNodeID string
	Phase        string
	Status       string
	ErrorMessage string

	// LvolID and the progress counters describe a volume's migration, and are
	// empty for a subsystem's: a group has no single volume whose snapshots
	// could be counted.
	LvolID        string
	RetryCount    int
	MaxRetries    int
	SnapsMigrated int
	SnapsTotal    int

	// TargetNQN, MemberCount, and ClusterID describe a subsystem's migration:
	// the subsystem the members land on, and how many there are.
	TargetNQN   string
	MemberCount int
	ClusterID   string
}

func volumeMigration(d cpapi.MigrationDTO) Migration {
	return Migration{
		Kind:          MigrationOfVolume,
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

func subsystemMigration(d cpapi.BatchMigrationDTO) Migration {
	return Migration{
		Kind:         MigrationOfSubsystem,
		ID:           d.Id.String(),
		SourceNodeID: d.SourceNodeId,
		TargetNodeID: d.TargetNodeId,
		Phase:        d.Phase,
		Status:       d.Status,
		ErrorMessage: d.ErrorMessage,
		TargetNQN:    d.TargetNqn,
		MemberCount:  d.MemberCount,
		ClusterID:    d.ClusterId,
	}
}

// migrationFromJSON reads whichever shape arrived.
//
// The kind is decided on a field the other shape does not have rather than on
// whether a decode succeeds, because both decode: JSON ignores the fields it
// does not recognize and zeroes the ones it does not find, so a group read as a
// volume's migration is a migration of volume "" with no snapshots, which reads
// as a finished one.
func migrationFromJSON(what string, raw json.RawMessage) (Migration, error) {
	var probe struct {
		LvolID      *string `json:"lvol_id"`
		MemberCount *int    `json:"member_count"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Migration{}, fmt.Errorf("%s: decode migration: %w", what, err)
	}

	switch {
	case probe.LvolID != nil:
		var d cpapi.MigrationDTO
		if err := json.Unmarshal(raw, &d); err != nil {
			return Migration{}, fmt.Errorf("%s: decode a volume's migration: %w", what, err)
		}
		return volumeMigration(d), nil
	case probe.MemberCount != nil:
		var d cpapi.BatchMigrationDTO
		if err := json.Unmarshal(raw, &d); err != nil {
			return Migration{}, fmt.Errorf("%s: decode a subsystem's migration: %w", what, err)
		}
		return subsystemMigration(d), nil
	default:
		return Migration{}, fmt.Errorf(
			"%s: neither a volume's migration, which carries lvol_id, nor a subsystem's, "+
				"which carries member_count", what)
	}
}

// ListMigrations returns the migrations of the subsystem with the given NQN,
// of both kinds, as the control plane returns them.
func (c *Client) ListMigrations(ctx context.Context, clusterID, nqn string) ([]Migration, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.ClustersSubsystemsMigrationsListApiV2ClustersClusterIdSubsystemsNqnMigrationsGetWithResponse(
		ctx, cluster, nqn)
	if err != nil {
		return nil, fmt.Errorf("list migrations of subsystem %s: %w", nqn, err)
	}
	what := "migrations of subsystem " + nqn
	raws, err := decodeBody[[]json.RawMessage](what, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]Migration, 0, len(raws))
	for _, raw := range raws {
		m, err := migrationFromJSON(what, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// GetMigration returns one migration of the subsystem with the given NQN. It
// wraps errs.ErrNotFound when the migration does not exist.
func (c *Client) GetMigration(ctx context.Context, clusterID, nqn, migrationID string) (Migration, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return Migration{}, err
	}
	migration, err := parseUUID("migration id", migrationID)
	if err != nil {
		return Migration{}, err
	}
	resp, err := c.api.ClustersSubsystemsMigrationsDetailApiV2ClustersClusterIdSubsystemsNqnMigrationsMigrationIdGetWithResponse(
		ctx, cluster, nqn, migration)
	if err != nil {
		return Migration{}, fmt.Errorf("get migration %s: %w", migrationID, err)
	}
	what := "migration " + migrationID
	raw, err := decodeBody[json.RawMessage](what, resp.StatusCode(), resp.Body)
	if err != nil {
		return Migration{}, err
	}
	return migrationFromJSON(what, raw)
}

// CancelMigration cancels a migration, of either kind.
func (c *Client) CancelMigration(ctx context.Context, clusterID, nqn, migrationID string) error {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return err
	}
	migration, err := parseUUID("migration id", migrationID)
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersSubsystemsMigrationsCancelApiV2ClustersClusterIdSubsystemsNqnMigrationsMigrationIdDeleteWithResponse(
		ctx, cluster, nqn, migration)
	if err != nil {
		return fmt.Errorf("cancel migration %s: %w", migrationID, err)
	}
	return migrationActionResult("cancel migration "+migrationID, resp.StatusCode(), resp.Body)
}

// ContinueMigration resumes a paused migration, of either kind, which is what a
// pre-created one is until something takes the next step.
func (c *Client) ContinueMigration(ctx context.Context, clusterID, nqn, migrationID string) error {
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

// CreateMigration starts migrating the subsystem with the given NQN to the
// target storage node and returns the migration that was created, whichever
// kind the control plane made of it.
func (c *Client) CreateMigration(ctx context.Context, clusterID, nqn, targetNodeID string) (Migration, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return Migration{}, err
	}
	target, err := parseUUID("target node id", targetNodeID)
	if err != nil {
		return Migration{}, err
	}
	// No response-format parameter: the control plane's own default is the full
	// body, which is the migration this returns.
	resp, err := c.api.ClustersSubsystemsMigrationsCreateApiV2ClustersClusterIdSubsystemsNqnMigrationsPostWithResponse(
		ctx, cluster, nqn, nil, cpapi.UnderscoreMigrationParams{TargetNodeId: target})
	if err != nil {
		return Migration{}, fmt.Errorf("create migration for subsystem %s: %w", nqn, err)
	}
	what := "create migration for subsystem " + nqn
	raw, err := decodeBody[json.RawMessage](what, resp.StatusCode(), resp.Body)
	if err != nil {
		return Migration{}, err
	}
	return migrationFromJSON(what, raw)
}

// migrationActionResult treats any 2xx as success for the fire-and-forget
// cancel and continue actions, whose bodies are untyped in the spec.
func migrationActionResult(what string, code int, body []byte) error {
	if code >= 200 && code < 300 {
		return nil
	}
	return respError(what, code, body)
}
