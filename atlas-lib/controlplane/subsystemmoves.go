// Consistency-group join and subsystem moves: the calls that bring a volume
// into its group and its namespace into the group's NVMe-oF subsystem
// (design-cg-live-subsystem-flip.md §9). They live beside the other group calls
// because both consumers address a group the same way, and the operator is the
// first to drive a move.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/ptr"
)

// The steps a join plan can name, in the order they are taken.
const (
	JoinStepMigrate  = "migrate"
	JoinStepJoin     = "join"
	JoinStepColocate = "colocate"
)

// Where a move's namespaces are exported. A move reports source until its
// namespaces have all left their subsystems, and target once they are all in
// the target.
const (
	NamespacesAtSource = "source"
	NamespacesAtTarget = "target"
)

// The refusals the control plane answers a join or a move with, as 409s. Each
// is its own sentinel because each asks something different of the caller: a
// membership refusal and a subsystem limit are final, a conflict is waited out,
// a passed deadline and a failed move are rolled back.
var (
	// ErrMembershipRefused is a join the group refused: the volume is in
	// another group or pool, a sibling is in another group, or the group is
	// full.
	ErrMembershipRefused = errors.New("the consistency group refused the volume")

	// ErrSubsystemLimitReached is a colocation that needs a fresh subsystem on
	// a node that already holds its maximum.
	ErrSubsystemLimitReached = errors.New("the node is at its subsystem limit")

	// ErrMoveConflict is a group already holding a move or a migration.
	ErrMoveConflict = errors.New("another move or migration holds the group")

	// ErrColocationRefused is a group that cannot be colocated at all, such as
	// one on a stretched cluster.
	ErrColocationRefused = errors.New("the group cannot be colocated")

	// ErrDeadlinePassed is a move asked to start after its hosts' deadline.
	// Nothing was touched.
	ErrDeadlinePassed = errors.New("the move's deadline has passed")

	// ErrMoveFailed is a move that failed partway. MoveRefusal.NamespacesAt
	// says where its namespaces are now.
	ErrMoveFailed = errors.New("the move failed")
)

// moveReasons maps the reason a 409 names to its sentinel.
var moveReasons = map[string]error{
	"SubsystemLimitReached": ErrSubsystemLimitReached,
	"MoveConflict":          ErrMoveConflict,
	"ColocationRefused":     ErrColocationRefused,
	"DeadlinePassed":        ErrDeadlinePassed,
	"MoveFailed":            ErrMoveFailed,
}

// MoveRefusal is a 409 the control plane explained. It unwraps to the sentinel
// its reason names, so a caller matches with errors.Is and reads the detail
// with errors.As.
type MoveRefusal struct {
	// Reason is the control plane's name for the refusal.
	Reason string
	// Message is the control plane's sentence for it.
	Message string
	// NodeID and Limit name the node and its subsystem limit, for
	// SubsystemLimitReached.
	NodeID string
	Limit  int
	// NamespacesAt is where the namespaces are, for MoveFailed.
	NamespacesAt string

	sentinel error
}

func (e *MoveRefusal) Error() string {
	if e.Message == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Message
}

func (e *MoveRefusal) Unwrap() error { return e.sentinel }

// refusal reads a 409 body. A detail object naming a known reason becomes a
// MoveRefusal, and a bare string detail becomes fallback with the string as
// its message: the members endpoints explain a refusal in prose only.
func refusal(what string, code int, body []byte, fallback error) error {
	if code != http.StatusConflict {
		return respError(what, code, body)
	}
	var wrapper struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && len(wrapper.Detail) > 0 {
		var detail struct {
			Reason       string `json:"reason"`
			Message      string `json:"message"`
			NodeID       string `json:"node_id"`
			Limit        int    `json:"limit"`
			NamespacesAt string `json:"namespaces_at"`
		}
		if json.Unmarshal(wrapper.Detail, &detail) == nil && detail.Reason != "" {
			sentinel, known := moveReasons[detail.Reason]
			if !known {
				sentinel = fallback
			}
			return fmt.Errorf("%s: %w", what, &MoveRefusal{
				Reason: detail.Reason, Message: detail.Message, NodeID: detail.NodeID,
				Limit: detail.Limit, NamespacesAt: detail.NamespacesAt, sentinel: sentinel,
			})
		}
		var message string
		if json.Unmarshal(wrapper.Detail, &message) == nil && fallback != nil {
			return fmt.Errorf("%s: %w: %s", what, fallback, message)
		}
	}
	if fallback != nil {
		return fmt.Errorf("%s: %w: %s", what, fallback, string(body))
	}
	return respError(what, code, body)
}

// Colocation is where a join's group would end up: the subsystem the group's
// members would share, whether it would be created, and which volumes would
// change subsystem to get there. Refusal names why it cannot, and is empty
// when it can.
type Colocation struct {
	TargetNQN     string
	TargetIsNew   bool
	MovingLvolIDs []string
	Refusal       string
}

// JoinPlan is what joining a volume to a group takes, decided by the control
// plane and reserving nothing.
type JoinPlan struct {
	// Steps are the steps in order, from JoinStepMigrate, JoinStepJoin, and
	// JoinStepColocate.
	Steps []string
	// TargetNodeID is the group's node, which a migrate step moves the volume
	// to.
	TargetNodeID string
	// MigrateLvolIDs are every volume the migration moves: the volume and those
	// sharing its subsystem.
	MigrateLvolIDs []string
	// TargetNQN is the group's subsystem.
	TargetNQN string
	// Colocation is nil when the group has no other member to colocate with.
	Colocation *Colocation
}

// Has reports whether the plan includes the step.
func (p JoinPlan) Has(step string) bool { return slices.Contains(p.Steps, step) }

// MovingNamespace is one volume whose namespace a move takes to the target.
type MovingNamespace struct {
	LvolID     string
	SourceNQN  string
	SourceNSID int
	// NSUUID and NGUID are the namespace's identity, which it keeps in the
	// target. A host finds the moved namespace by them.
	NSUUID     string
	NGUID      string
	TargetNSID int
}

// SubsystemMove is a group's move into one subsystem.
type SubsystemMove struct {
	ID           string
	RequestID    string
	TargetNQN    string
	TargetIsNew  bool
	NamespacesAt string
	// Deadline is the hosts' suspend deadline the move was started under, and
	// zero before it started.
	Deadline time.Time
	Status   string
	Volumes  []MovingNamespace
}

// Empty reports a move with nothing to move: the volume is already in the
// group's subsystem, or the group has no other member.
func (m SubsystemMove) Empty() bool { return m.ID == "" && len(m.Volumes) == 0 }

// PreparedMove is a move whose target exists on every node of the group, with
// the paths a host connects to reach it.
type PreparedMove struct {
	Move   SubsystemMove
	Target lvol.Connection
}

// PlanConsistencyGroupJoin asks what joining lvolID to the group takes. It
// reserves nothing, so the answer is a preview: a move created afterward
// decides again against the state at that moment.
func (c *Client) PlanConsistencyGroupJoin(ctx context.Context, clusterID, groupID, lvolID string) (JoinPlan, error) {
	cluster, group, err := groupPath(clusterID, groupID)
	if err != nil {
		return JoinPlan{}, err
	}
	what := fmt.Sprintf("plan joining volume %s to consistency group %s", lvolID, groupID)
	resp, err := c.api.ClustersConsistencyGroupsMembersPlanApiV2ClustersClusterIdConsistencyGroupsGroupIdMembersPlanPostWithResponse(
		ctx, cluster, group, cpapi.ConsistencyGroupMemberJoinDTO{LvolId: lvolID})
	if err != nil {
		return JoinPlan{}, fmt.Errorf("%s: %w", what, err)
	}
	if resp.JSON200 == nil {
		return JoinPlan{}, refusal(what, resp.StatusCode(), resp.Body, ErrMembershipRefused)
	}
	dto := resp.JSON200
	plan := JoinPlan{
		Steps:          dto.Steps,
		TargetNodeID:   ptr.From(dto.TargetNodeId, ""),
		MigrateLvolIDs: ptr.From(dto.MigrateLvolIds, nil),
		TargetNQN:      ptr.From(dto.TargetNqn, ""),
	}
	if co := dto.Colocation; co != nil {
		plan.Colocation = &Colocation{
			TargetNQN:     ptr.From(co.TargetNqn, ""),
			TargetIsNew:   ptr.BoolFromOrFalse(co.TargetIsNew),
			MovingLvolIDs: ptr.From(co.MovingLvolIds, nil),
			Refusal:       ptr.From(co.Refusal, ""),
		}
	}
	return plan, nil
}

// JoinConsistencyGroupMember adds lvolID to the group. A refusal wraps
// ErrMembershipRefused.
func (c *Client) JoinConsistencyGroupMember(ctx context.Context, clusterID, groupID, lvolID string) error {
	cluster, group, err := groupPath(clusterID, groupID)
	if err != nil {
		return err
	}
	what := fmt.Sprintf("join volume %s to consistency group %s", lvolID, groupID)
	resp, err := c.api.ClustersConsistencyGroupsMembersJoinApiV2ClustersClusterIdConsistencyGroupsGroupIdMembersPostWithResponse(
		ctx, cluster, group, cpapi.ConsistencyGroupMemberJoinDTO{LvolId: lvolID})
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if resp.JSON200 == nil {
		return refusal(what, resp.StatusCode(), resp.Body, ErrMembershipRefused)
	}
	return nil
}

// CreateSubsystemMove decides the subsystem the group ends up in once lvolID
// joins it, and reserves the slots there. requestID makes the call idempotent:
// a repeat with the same id is answered with the move it created.
func (c *Client) CreateSubsystemMove(
	ctx context.Context, clusterID, groupID, lvolID, requestID string,
) (SubsystemMove, error) {
	what := fmt.Sprintf("create a subsystem move of consistency group %s for volume %s", groupID, lvolID)
	return c.createMove(ctx, what, clusterID, groupID, cpapi.ConsistencyGroupSubsystemMoveCreateDTO{
		LvolId: lvolID, RequestId: requestID,
	})
}

// CreateSubsystemLeave splits lvolID, a member about to leave the group, out of
// the subsystem it shares with the group's other members into a fresh one on
// its node, and reserves it. The move runs through the same prepare, move, and
// commit calls as a join's. It is Empty when the volume shares its subsystem
// with nobody, and requestID makes it idempotent as CreateSubsystemMove's is.
func (c *Client) CreateSubsystemLeave(
	ctx context.Context, clusterID, groupID, lvolID, requestID string,
) (SubsystemMove, error) {
	what := fmt.Sprintf("create a subsystem move of volume %s out of consistency group %s", lvolID, groupID)
	return c.createMove(ctx, what, clusterID, groupID, cpapi.ConsistencyGroupSubsystemMoveCreateDTO{
		LvolId: lvolID, RequestId: requestID, Purpose: ptr.To(cpapi.Leave),
	})
}

func (c *Client) createMove(
	ctx context.Context, what, clusterID, groupID string, body cpapi.ConsistencyGroupSubsystemMoveCreateDTO,
) (SubsystemMove, error) {
	cluster, group, err := groupPath(clusterID, groupID)
	if err != nil {
		return SubsystemMove{}, err
	}
	resp, err := c.api.ClustersConsistencyGroupsSubsystemMovesCreateApiV2ClustersClusterIdConsistencyGroupsGroupIdSubsystemMovesPostWithResponse(
		ctx, cluster, group, body)
	if err != nil {
		return SubsystemMove{}, fmt.Errorf("%s: %w", what, err)
	}
	if resp.JSON200 == nil {
		return SubsystemMove{}, refusal(what, resp.StatusCode(), resp.Body, nil)
	}
	return moveOf(*resp.JSON200), nil
}

// DetachConsistencyGroupMember closes lvolID's membership of the group. The
// control plane answers a volume that is not a member as it answers one that
// is, and a group that is gone has no membership left to close, so a retried
// detach succeeds.
func (c *Client) DetachConsistencyGroupMember(ctx context.Context, clusterID, groupID, lvolID string) error {
	cluster, group, err := groupPath(clusterID, groupID)
	if err != nil {
		return err
	}
	what := fmt.Sprintf("detach volume %s from consistency group %s", lvolID, groupID)
	resp, err := c.api.ClustersConsistencyGroupsMembersDetachApiV2ClustersClusterIdConsistencyGroupsGroupIdMembersLvolIdDeleteWithResponse(
		ctx, cluster, group, lvolID)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		return nil
	}
	return noContent(what, resp.StatusCode(), resp.Body)
}

// GetSubsystemMove reads a move. An unknown move wraps errs.ErrNotFound.
func (c *Client) GetSubsystemMove(ctx context.Context, clusterID, groupID, moveID string) (SubsystemMove, error) {
	cluster, group, err := groupPath(clusterID, groupID)
	if err != nil {
		return SubsystemMove{}, err
	}
	what := fmt.Sprintf("subsystem move %s of consistency group %s", moveID, groupID)
	resp, err := c.api.ClustersConsistencyGroupsSubsystemMovesGetApiV2ClustersClusterIdConsistencyGroupsGroupIdSubsystemMovesMoveIdGetWithResponse(
		ctx, cluster, group, moveID)
	if err != nil {
		return SubsystemMove{}, fmt.Errorf("%s: %w", what, err)
	}
	move, err := decodeBody[cpapi.ConsistencyGroupSubsystemMoveDTO](what, resp.StatusCode(), resp.Body)
	if err != nil {
		return SubsystemMove{}, err
	}
	return moveOf(move), nil
}

// PrepareSubsystemMove creates the move's target on every node of the group when
// it is new, and returns the paths a host connects to reach it. hostNQN names
// the host the paths are resolved for, which an access-controlled subsystem
// needs. Repeating the call creates nothing new.
func (c *Client) PrepareSubsystemMove(
	ctx context.Context, clusterID, groupID, moveID, hostNQN string,
) (PreparedMove, error) {
	cluster, group, err := groupPath(clusterID, groupID)
	if err != nil {
		return PreparedMove{}, err
	}
	what := fmt.Sprintf("prepare subsystem move %s of consistency group %s", moveID, groupID)
	body := cpapi.ConsistencyGroupSubsystemMovePrepareDTO{}
	if hostNQN != "" {
		body.HostNqn = &hostNQN
	}
	resp, err := c.api.ClustersConsistencyGroupsSubsystemMovesPrepareApiV2ClustersClusterIdConsistencyGroupsGroupIdSubsystemMovesMoveIdPreparePostWithResponse(
		ctx, cluster, group, moveID, body)
	if err != nil {
		return PreparedMove{}, fmt.Errorf("%s: %w", what, err)
	}
	if resp.JSON200 == nil {
		return PreparedMove{}, refusal(what, resp.StatusCode(), resp.Body, nil)
	}
	move := moveOf(resp.JSON200.Move)
	target := lvol.Connection{NQN: move.TargetNQN}
	if raw := resp.JSON200.Connect; raw != nil {
		// The entries are untyped in the spec and are the same model the
		// volume connect route answers with, so they are read as that.
		encoded, err := json.Marshal(*raw)
		if err != nil {
			return PreparedMove{}, fmt.Errorf("%s: encode the target paths: %w", what, err)
		}
		var entries []cpapi.NvmeConnectEntry
		if err := json.Unmarshal(encoded, &entries); err != nil {
			return PreparedMove{}, fmt.Errorf("%s: decode the target paths: %w", what, err)
		}
		for _, e := range entries {
			if target.NQN == "" {
				target.NQN = e.Nqn
			}
			target.Endpoints = append(target.Endpoints, endpointOf(e))
		}
	}
	return PreparedMove{Move: move, Target: target}, nil
}

// StartSubsystemMove moves every namespace of the move into the target, under
// the hosts' suspend deadline. The control plane refuses with ErrDeadlinePassed
// before touching anything once the deadline is past, and a move that fails
// partway answers ErrMoveFailed saying where its namespaces are. A repeat after
// success answers with the move at the target.
func (c *Client) StartSubsystemMove(
	ctx context.Context, clusterID, groupID, moveID string, deadline time.Time,
) (SubsystemMove, error) {
	cluster, group, err := groupPath(clusterID, groupID)
	if err != nil {
		return SubsystemMove{}, err
	}
	what := fmt.Sprintf("move the namespaces of subsystem move %s of consistency group %s", moveID, groupID)
	resp, err := c.api.ClustersConsistencyGroupsSubsystemMovesMoveApiV2ClustersClusterIdConsistencyGroupsGroupIdSubsystemMovesMoveIdMovePostWithResponse(
		ctx, cluster, group, moveID, cpapi.ConsistencyGroupSubsystemMoveStartDTO{Deadline: int(deadline.Unix())})
	if err != nil {
		return SubsystemMove{}, fmt.Errorf("%s: %w", what, err)
	}
	move, err := decodeBody[cpapi.ConsistencyGroupSubsystemMoveDTO](what, resp.StatusCode(), resp.Body)
	if err != nil {
		return SubsystemMove{}, refusal(what, resp.StatusCode(), resp.Body, nil)
	}
	return moveOf(move), nil
}

// CommitSubsystemMove records every moved volume in the target and deletes the
// subsystems the move emptied. It is idempotent.
func (c *Client) CommitSubsystemMove(ctx context.Context, clusterID, groupID, moveID string) error {
	cluster, group, err := groupPath(clusterID, groupID)
	if err != nil {
		return err
	}
	what := fmt.Sprintf("commit subsystem move %s of consistency group %s", moveID, groupID)
	resp, err := c.api.ClustersConsistencyGroupsSubsystemMovesCommitApiV2ClustersClusterIdConsistencyGroupsGroupIdSubsystemMovesMoveIdCommitPostWithResponse(
		ctx, cluster, group, moveID)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return noContent(what, resp.StatusCode(), resp.Body)
}

// RollbackSubsystemMove puts every namespace back in its source, deletes an
// empty fresh target, and releases the move's reservation. It is idempotent.
func (c *Client) RollbackSubsystemMove(ctx context.Context, clusterID, groupID, moveID string) error {
	cluster, group, err := groupPath(clusterID, groupID)
	if err != nil {
		return err
	}
	what := fmt.Sprintf("roll back subsystem move %s of consistency group %s", moveID, groupID)
	resp, err := c.api.ClustersConsistencyGroupsSubsystemMovesRollbackApiV2ClustersClusterIdConsistencyGroupsGroupIdSubsystemMovesMoveIdRollbackPostWithResponse(
		ctx, cluster, group, moveID)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return noContent(what, resp.StatusCode(), resp.Body)
}

// noContent maps a response whose success carries no body.
func noContent(what string, code int, body []byte) error {
	if code >= 200 && code < 300 {
		return nil
	}
	return refusal(what, code, body, nil)
}

// groupPath parses the cluster and group a group endpoint is addressed by.
func groupPath(clusterID, groupID string) (cluster, group uuid.UUID, err error) {
	if cluster, err = parseUUID("cluster id", clusterID); err != nil {
		return cluster, group, err
	}
	group, err = groupUUID(groupID)
	return cluster, group, err
}

func moveOf(dto cpapi.ConsistencyGroupSubsystemMoveDTO) SubsystemMove {
	move := SubsystemMove{
		ID:           dto.Id,
		RequestID:    ptr.From(dto.RequestId, ""),
		TargetNQN:    ptr.From(dto.TargetNqn, ""),
		TargetIsNew:  ptr.BoolFromOrFalse(dto.TargetIsNew),
		NamespacesAt: ptr.From(dto.NamespacesAt, ""),
		Status:       ptr.From(dto.Status, ""),
	}
	if seconds := ptr.From(dto.Deadline, 0); seconds > 0 {
		move.Deadline = time.Unix(int64(seconds), 0).UTC()
	}
	for _, v := range ptr.From(dto.Volumes, nil) {
		move.Volumes = append(move.Volumes, MovingNamespace{
			LvolID:     v.LvolId,
			SourceNQN:  ptr.From(v.SourceNqn, ""),
			SourceNSID: ptr.From(v.SourceNsid, 0),
			NSUUID:     ptr.From(v.NsUuid, ""),
			NGUID:      ptr.From(v.Nguid, ""),
			TargetNSID: ptr.From(v.TargetNsid, 0),
		})
	}
	return move
}
