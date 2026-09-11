// Tests for the ControlPlane reconciler: the FDB readiness probe it runs and the
// phase it records from the result.
//
// They live here rather than beside another controller's tests because the
// singleton ControlPlane is the one object this reconciler owns, and its phase
// vocabulary is version-specific: v1alpha2 says Available where v1alpha1 said
// Ready, so a test asserting the wrong word passes against a status the API
// server would refuse.

package controller

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
)

// Regression: 2026-09-11-controlplane-reconciler-reads-retired-version. The
// reconciler read and watched the singleton at v1alpha1 after v1alpha2 became
// the stored version. A fresh install deploys no conversion webhook, so the
// object it owns was invisible to it and no phase was ever recorded. The phase it records is asserted alongside, because v1alpha2's Enum
// admits Available and not the Ready this wrote.
func TestControlPlaneReconcileRecordsAvailablePhase(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", false)
	defer mock.Close()
	mock.Register(http.MethodGet, "/api/v2/_meta/ready", webapimock.RouteResponse{
		Status:  http.StatusOK,
		Body:    `{"status":"ok"}`,
		Headers: map[string]string{"Content-Type": "application/json"},
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	cp := &simplyblockv1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SingletonControlPlaneName,
			Namespace: "simplyblock",
		},
	}

	scheme := newTestScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha2.ControlPlane{}).
		WithObjects(cp).
		Build()

	r := &ControlPlaneReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(8),
	}

	key := client.ObjectKeyFromObject(cp)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	var got simplyblockv1alpha2.ControlPlane
	if err := cl.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("reading back the ControlPlane: %v", err)
	}
	if got.Status.Phase != controlPlanePhaseAvailable {
		t.Fatalf("expected phase %q, got %q", controlPlanePhaseAvailable, got.Status.Phase)
	}
	if got.Status.LastChecked == nil {
		t.Fatal("expected the probe to stamp status.lastChecked")
	}
}

// recordingSink is a logr sink that keeps what was logged at info level, so a
// test can assert what a steady state is quiet about. Only the levels this
// controller uses are recorded; everything else is discarded.
type recordingSink struct {
	verbosity int
	messages  *[]string
}

func (s recordingSink) Init(logr.RuntimeInfo)        {}
func (s recordingSink) Enabled(int) bool             { return true }
func (s recordingSink) WithName(string) logr.LogSink { return s }

func (s recordingSink) Info(level int, msg string, _ ...any) {
	// controller-runtime logs debug at V(1) and above, and info at V(0).
	if level+s.verbosity == 0 {
		*s.messages = append(*s.messages, msg)
	}
}

func (s recordingSink) Error(_ error, msg string, _ ...any) {
	*s.messages = append(*s.messages, msg)
}

func (s recordingSink) WithValues(...any) logr.LogSink { return s }

func (s recordingSink) V(level int) logr.LogSink {
	return recordingSink{verbosity: s.verbosity + level, messages: s.messages}
}

// Regression: 2026-09-11-controlplane-logs-every-probe. The probe repeats every
// 30 seconds for the life of the cluster, and a readiness that has not changed
// was announced at info on every one of them: about six thousand lines a day per
// operator, all of them saying what the line before said. The transition is the
// event, and it is what the log says too.
func TestARepeatedReadyProbeIsNotAnnouncedAgain(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", false)
	defer mock.Close()
	mock.Register(http.MethodGet, "/api/v2/_meta/ready", webapimock.RouteResponse{
		Status:  http.StatusOK,
		Body:    `{"status":"ok"}`,
		Headers: map[string]string{"Content-Type": "application/json"},
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	cp := &simplyblockv1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SingletonControlPlaneName,
			Namespace: "simplyblock",
		},
	}
	scheme := newTestScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha2.ControlPlane{}).
		WithObjects(cp).
		Build()
	r := &ControlPlaneReconciler{Client: cl, Scheme: scheme, Recorder: events.NewFakeRecorder(8)}

	var logged []string
	ctx := logf.IntoContext(context.Background(), logr.New(recordingSink{messages: &logged}))
	key := client.ObjectKeyFromObject(cp)

	for i := range 3 {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	var announcements int
	for _, msg := range logged {
		if msg == "control plane ready" {
			announcements++
		}
	}
	if announcements != 1 {
		t.Fatalf("expected the readiness to be announced once, got %d in %v", announcements, logged)
	}
}
