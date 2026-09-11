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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
