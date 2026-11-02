package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/errs"
)

const (
	testGroup  = "c9c9c9c9-c9c9-4c9c-8c9c-c9c9c9c9c9c9"
	testLvol   = "a1111111-1111-4111-8111-111111111111"
	testMoveID = "move-7"
	testTarget = "nqn.2023-02.io.simplyblock:cluster:lvol:target"
	testSource = "nqn.2023-02.io.simplyblock:cluster:lvol:source"
	groupBase  = "/api/v2/clusters/" + testCluster + "/consistency-groups/" + testGroup
)

func moveJSON(namespacesAt string) string {
	return `{"id":"` + testMoveID + `","request_id":"req-1","target_nqn":"` + testTarget + `",` +
		`"target_is_new":true,"namespaces_at":"` + namespacesAt + `","deadline":0,"status":"reserved",` +
		`"volumes":[{"lvol_id":"` + testLvol + `","source_nqn":"` + testSource + `","source_nsid":1,` +
		`"ns_uuid":"` + testLvol + `","nguid":"00ff","target_nsid":3}]}`
}

func conflict(w http.ResponseWriter, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_, _ = w.Write([]byte(`{"detail":` + detail + `}`))
}

func TestPlanConsistencyGroupJoinReadsTheColocationOutcome(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != groupBase+"/members/plan" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), testLvol) {
			t.Errorf("body %s does not name the volume", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"steps":["migrate","join","colocate"],"target_node_id":"n-1",` +
			`"migrate_lvol_ids":["` + testLvol + `"],"target_nqn":"` + testTarget + `",` +
			`"colocation":{"target_nqn":"` + testTarget + `","target_is_new":true,` +
			`"moving_lvol_ids":["` + testLvol + `"],"refusal":""}}`))
	})

	plan, err := c.PlanConsistencyGroupJoin(context.Background(), testCluster, testGroup, testLvol)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Has(JoinStepMigrate) || !plan.Has(JoinStepJoin) || plan.TargetNodeID != "n-1" {
		t.Errorf("plan = %+v, want migrate and join to node n-1", plan)
	}
	if plan.Colocation == nil || !plan.Colocation.TargetIsNew || plan.Colocation.TargetNQN != testTarget ||
		len(plan.Colocation.MovingLvolIDs) != 1 {
		t.Errorf("colocation = %+v, want a fresh target moving one volume", plan.Colocation)
	}
}

func TestPlanConsistencyGroupJoinWithNoOtherMemberHasNoColocation(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"steps":["join"],"colocation":null}`))
	})

	plan, err := c.PlanConsistencyGroupJoin(context.Background(), testCluster, testGroup, testLvol)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Colocation != nil || plan.Has(JoinStepMigrate) {
		t.Errorf("plan = %+v, want a bare join", plan)
	}
}

func TestARefusedJoinPlanIsAMembershipRefusal(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		conflict(w, `"volume is in another consistency group"`)
	})

	_, err := c.PlanConsistencyGroupJoin(context.Background(), testCluster, testGroup, testLvol)
	if !errors.Is(err, ErrMembershipRefused) {
		t.Fatalf("err = %v, want ErrMembershipRefused", err)
	}
	if !strings.Contains(err.Error(), "another consistency group") {
		t.Errorf("err = %v, want the control plane's reason", err)
	}
}

func TestJoinConsistencyGroupMemberPostsTheVolume(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != groupBase+"/members" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lvol_id":"` + testLvol + `","joined_seq":4,"removed_seq":0,` +
			`"node_id":"n","lvs_name":"l","online":true}`))
	})

	if err := c.JoinConsistencyGroupMember(context.Background(), testCluster, testGroup, testLvol); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestARefusedJoinIsAMembershipRefusal(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		conflict(w, `"the group is full"`)
	})

	err := c.JoinConsistencyGroupMember(context.Background(), testCluster, testGroup, testLvol)
	if !errors.Is(err, ErrMembershipRefused) {
		t.Fatalf("err = %v, want ErrMembershipRefused", err)
	}
}

func TestCreateSubsystemMoveSendsTheRequestIDAndReadsTheMove(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != groupBase+"/subsystem-moves" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["lvol_id"] != testLvol || body["request_id"] != "req-1" {
			t.Errorf("body = %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(moveJSON("source")))
	})

	move, err := c.CreateSubsystemMove(context.Background(), testCluster, testGroup, testLvol, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if move.ID != testMoveID || move.TargetNQN != testTarget || !move.TargetIsNew ||
		move.NamespacesAt != NamespacesAtSource {
		t.Errorf("move = %+v", move)
	}
	if len(move.Volumes) != 1 {
		t.Fatalf("volumes = %+v, want one", move.Volumes)
	}
	v := move.Volumes[0]
	if v.LvolID != testLvol || v.SourceNQN != testSource || v.SourceNSID != 1 || v.TargetNSID != 3 ||
		v.NSUUID != testLvol {
		t.Errorf("volume = %+v", v)
	}
}

func TestAMoveWithNothingToMoveIsEmpty(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"","volumes":[]}`))
	})

	move, err := c.CreateSubsystemMove(context.Background(), testCluster, testGroup, testLvol, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if !move.Empty() {
		t.Errorf("move = %+v, want empty", move)
	}
}

func TestTheSubsystemLimitRefusalCarriesTheNodeAndTheLimit(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		conflict(w, `{"reason":"SubsystemLimitReached","message":"node n-1 holds its maximum of 500 subsystems",`+
			`"node_id":"n-1","limit":500}`)
	})

	_, err := c.CreateSubsystemMove(context.Background(), testCluster, testGroup, testLvol, "req-1")
	if !errors.Is(err, ErrSubsystemLimitReached) {
		t.Fatalf("err = %v, want ErrSubsystemLimitReached", err)
	}
	var refusal *MoveRefusal
	if !errors.As(err, &refusal) || refusal.NodeID != "n-1" || refusal.Limit != 500 {
		t.Errorf("refusal = %+v, want node n-1 and limit 500", refusal)
	}
}

func TestEveryMoveRefusalReasonIsItsOwnSentinel(t *testing.T) {
	for reason, want := range map[string]error{
		"MoveConflict":      ErrMoveConflict,
		"ColocationRefused": ErrColocationRefused,
		"DeadlinePassed":    ErrDeadlinePassed,
		"MoveFailed":        ErrMoveFailed,
	} {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			conflict(w, `{"reason":"`+reason+`","message":"m","namespaces_at":"source"}`)
		})
		_, err := c.StartSubsystemMove(context.Background(), testCluster, testGroup, testMoveID, time.Unix(100, 0))
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", reason, err, want)
		}
	}
}

func TestAFailedMoveSaysWhereTheNamespacesAre(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		conflict(w, `{"reason":"MoveFailed","message":"add failed","namespaces_at":"source"}`)
	})

	_, err := c.StartSubsystemMove(context.Background(), testCluster, testGroup, testMoveID, time.Unix(100, 0))
	var refusal *MoveRefusal
	if !errors.As(err, &refusal) || refusal.NamespacesAt != NamespacesAtSource {
		t.Errorf("err = %v, want a refusal naming the source", err)
	}
}

func TestStartSubsystemMoveSendsTheDeadlineInUnixSeconds(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != groupBase+"/subsystem-moves/"+testMoveID+"/move" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]int64
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["deadline"] != 1_800_000_000 {
			t.Errorf("deadline = %d", body["deadline"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(moveJSON("target")))
	})

	move, err := c.StartSubsystemMove(context.Background(), testCluster, testGroup, testMoveID,
		time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if move.NamespacesAt != NamespacesAtTarget {
		t.Errorf("namespaces at %q, want target", move.NamespacesAt)
	}
}

func TestPrepareSubsystemMoveReturnsTheTargetPaths(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != groupBase+"/subsystem-moves/"+testMoveID+"/prepare" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"move":` + moveJSON("source") + `,"connect":[` +
			`{"transport":"tcp","ip":"10.0.0.1","port":4420,"nqn":"` + testTarget + `","reconnect_delay":2,` +
			`"ctrl_loss_tmo":60,"fast_io_fail_tmo":1,"nr_io_queues":4,"keep_alive_tmo":5,"connect":"nvme connect"},` +
			`{"transport":"tcp","ip":"10.0.0.2","port":4420,"nqn":"` + testTarget + `","reconnect_delay":2,` +
			`"ctrl_loss_tmo":60,"fast_io_fail_tmo":1,"nr_io_queues":4,"keep_alive_tmo":5,"connect":"nvme connect"}]}`))
	})

	prepared, err := c.PrepareSubsystemMove(context.Background(), testCluster, testGroup, testMoveID, "")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Target.NQN != testTarget || len(prepared.Target.Endpoints) != 2 ||
		prepared.Target.Endpoints[0].Address != "10.0.0.1" {
		t.Errorf("target = %+v, want both paths to the target", prepared.Target)
	}
	if prepared.Move.ID != testMoveID {
		t.Errorf("move = %+v", prepared.Move)
	}
}

func TestCommitAndRollbackAnswerWithNoContent(t *testing.T) {
	var paths []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.CommitSubsystemMove(context.Background(), testCluster, testGroup, testMoveID); err != nil {
		t.Fatal(err)
	}
	if err := c.RollbackSubsystemMove(context.Background(), testCluster, testGroup, testMoveID); err != nil {
		t.Fatal(err)
	}
	want := []string{
		groupBase + "/subsystem-moves/" + testMoveID + "/commit",
		groupBase + "/subsystem-moves/" + testMoveID + "/rollback",
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

func TestAnUnknownMoveIsNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"no such move"}`))
	})

	_, err := c.GetSubsystemMove(context.Background(), testCluster, testGroup, testMoveID)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if err := c.CommitSubsystemMove(context.Background(), testCluster, testGroup, testMoveID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("commit err = %v, want ErrNotFound", err)
	}
}
