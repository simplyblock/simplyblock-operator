// Unit tests for the convergence verdict a migration plan carries: a source
// lvstore whose volumes are written at least as fast as it replicates never
// reaches a cutover, so the operation says so instead of promising an ETA,
// warns once per transition, and mirrors the plan's planning-time warnings.

package controller

import (
	"net/http"
	"strings"
	"testing"

	"k8s.io/client-go/tools/events"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// planJSONWith is planJSON for the running plan testPlanID with an explicit
// forecast block and warnings.
func planJSONWith(forecast, warnings string, volumes ...string) string {
	return `{"id":"` + testPlanID + `","status":"` + migrationPlanRunning + `","volumes":[` + strings.Join(volumes, ",") +
		`],"forecast":` + forecast + `,"warnings":` + warnings + `}`
}

const notConvergingForecast = `{"bytes_remaining":85899345920,"eta_seconds":0,"completes_at":0,` +
	`"throughput_bytes_per_sec":104857600,"throughput_measured":true,"converging":false,` +
	`"dirty_bytes_per_sec":157286400,"not_converging_lvs":["LVS_1"]}`

const convergingForecast = `{"bytes_remaining":1073741824,"eta_seconds":120,"completes_at":0,` +
	`"throughput_bytes_per_sec":104857600,"throughput_measured":true,"converging":true,` +
	`"dirty_bytes_per_sec":1048576,"not_converging_lvs":[]}`

func drainEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func countEvents(evs []string, reason string) int {
	n := 0
	for _, e := range evs {
		if strings.Contains(e, " "+reason+" ") {
			n++
		}
	}
	return n
}

func TestMigration_NotConverging_SaysSoAndWarnsOncePerTransition(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusCreated, planJSONWith(notConvergingForecast, `[]`,
			planVolumeJSON("vol-u", "replicating", 0))
	}}
	ps.planBody = planJSONWith(notConvergingForecast, `[]`,
		planVolumeJSON("vol-u", "replicating", 0))
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), slotForPolicy(),
		migrationOps(utils.ReplicationOpsScopePolicy, "pol"))
	rec := events.NewFakeRecorder(64)
	r.Recorder = rec
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)
	reconcileOps(t, r)
	reconcileOps(t, r)

	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseRunning) {
		t.Fatalf("phase = %q, want Running: not converging is reported, not failed", got.Status.Phase)
	}
	f := got.Status.Forecast
	if f == nil || f.Converging == nil || *f.Converging {
		t.Fatalf("forecast = %+v, want converging=false", f)
	}
	if f.DirtyBytesPerSec != 157286400 || len(f.NotConvergingLVS) != 1 || f.NotConvergingLVS[0] != "LVS_1" {
		t.Errorf("forecast = %+v, want the dirty rate and LVS_1", f)
	}
	if !strings.Contains(got.Status.Message, "Not converging") || strings.Contains(got.Status.Message, "left") {
		t.Errorf("message = %q, want a not-converging verdict and no ETA", got.Status.Message)
	}
	if n := countEvents(drainEvents(rec), "NotConverging"); n != 1 {
		t.Errorf("NotConverging events = %d, want exactly 1 across three passes", n)
	}

	// Writes slow down: the verdict flips back, with one Converging event.
	ps.planBody = planJSONWith(convergingForecast, `[]`,
		planVolumeJSON("vol-u", "replicating", 120))
	reconcileOps(t, r)
	got = getOps(t, cl)
	if got.Status.Forecast.Converging == nil || !*got.Status.Forecast.Converging {
		t.Errorf("forecast = %+v, want converging=true again", got.Status.Forecast)
	}
	if n := countEvents(drainEvents(rec), "Converging"); n != 1 {
		t.Errorf("Converging events = %d, want 1 on the transition back", n)
	}
}

func TestMigration_PlanWarningsAreMirrored(t *testing.T) {
	warnings := `["source cluster has 100 GiB free, less than the 240 GiB pin"]`
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusCreated, planJSONWith(convergingForecast, warnings,
			planVolumeJSON("vol-u", "replicating", 120))
	}}
	ps.planBody = planJSONWith(convergingForecast, warnings,
		planVolumeJSON("vol-u", "replicating", 120))
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), slotForPolicy(),
		migrationOps(utils.ReplicationOpsScopePolicy, "pol"))
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)
	reconcileOps(t, r)

	got := getOps(t, cl)
	if len(got.Status.Warnings) != 1 || !strings.Contains(got.Status.Warnings[0], "240 GiB") {
		t.Errorf("warnings = %v, want the plan's warning", got.Status.Warnings)
	}
}

func TestMigration_OlderBackendLeavesConvergenceUnknown(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusCreated, planJSON(testPlanID, migrationPlanRunning, planVolumeJSON("vol-u", "replicating", 600))
	}}
	ps.planBody = planJSON(testPlanID, migrationPlanRunning, planVolumeJSON("vol-u", "replicating", 600))
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), slotForPolicy(),
		migrationOps(utils.ReplicationOpsScopePolicy, "pol"))
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)
	reconcileOps(t, r)

	got := getOps(t, cl)
	if got.Status.Forecast == nil || got.Status.Forecast.Converging != nil {
		t.Errorf("forecast = %+v, want converging unknown (nil) from a backend without the verdict", got.Status.Forecast)
	}
	if !strings.Contains(got.Status.Message, "left") {
		t.Errorf("message = %q, want the ETA as before", got.Status.Message)
	}
}
