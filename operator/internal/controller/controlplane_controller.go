/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// SingletonControlPlaneName is the fixed name of the singleton ControlPlane CR
// created by the Helm chart. The controller ignores any CR with a different name.
const SingletonControlPlaneName = "simplyblock"

// controlPlaneRequeueInterval is how often the FDB health check is repeated.
const controlPlaneRequeueInterval = 30 * time.Second

// The phases this controller records. They are v1alpha2's vocabulary, whose Enum
// admits Available where v1alpha1 said Ready, and they live beside the reconciler
// that writes them rather than among the shared cluster constants: no other
// kind's phase is spelled from this pair.
const (
	controlPlanePhaseInitializing = "Initializing"
	controlPlanePhaseAvailable    = "Available"
)

const (
	// eventReasonFDBReady is emitted when the FDB health check recovers after a
	// prior failure (phase transitions from Initializing → Ready).
	eventReasonCPFDBReady = "FDBReady"

	// eventReasonFDBNotReady is emitted when the FDB health check first fails
	// (phase transitions from Ready → Initializing, or on the initial probe).
	eventReasonCPFDBNotReady = "FDBNotReady"
)

// ControlPlaneReconciler reconciles the singleton ControlPlane object.
type ControlPlaneReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplanes/status,verbs=get;update;patch

func (r *ControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if req.Name != SingletonControlPlaneName {
		log.Info("ignoring non-singleton ControlPlane CR", "name", req.Name)
		return ctrl.Result{}, nil
	}

	// v1alpha2 is the stored version. Reading the singleton at v1alpha1 would be
	// answered only by the conversion webhook, which a fresh install does not
	// deploy, and the cache backing this read lists empty rather than failing:
	// the reconciler would silently own an object it never sees.
	cp := &simplyblockv1alpha2.ControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, cp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	now := metav1.Now()
	orig := cp.DeepCopy()

	prevPhase := cp.Status.Phase

	apiClient := webapi.NewClient()
	body, status, err := apiClient.Do(ctx, http.MethodGet, "/api/v2/_meta/ready", nil)
	if err != nil || status >= 300 {
		msg := string(body)
		if err != nil {
			msg = err.Error()
		} else {
			msg = fmt.Sprintf("status=%d: %s", status, msg)
		}
		// The probe repeats for the life of the cluster, so a readiness that has
		// not changed is logged at debug. What is worth an operator's attention
		// is the transition, which is also what the event below reports.
		if prevPhase != controlPlanePhaseInitializing {
			log.Info("control plane not ready", "reason", msg)
		} else {
			log.V(1).Info("control plane still not ready", "reason", msg)
		}

		cp.Status.Phase = controlPlanePhaseInitializing
		cp.Status.Message = msg
		cp.Status.LastChecked = &now

		if err := r.Status().Patch(ctx, cp, client.MergeFrom(orig)); err != nil {
			log.Error(err, "failed to patch ControlPlane status")
		}

		// Only emit event on transition to avoid spamming every 30 s.
		if prevPhase != controlPlanePhaseInitializing {
			r.Recorder.Eventf(cp, nil, corev1.EventTypeWarning, eventReasonCPFDBNotReady, eventReasonCPFDBNotReady, "FDB health check failed: %s", msg)
		}
		return ctrl.Result{RequeueAfter: controlPlaneRequeueInterval}, nil
	}

	cp.Status.Phase = controlPlanePhaseAvailable
	cp.Status.Message = ""
	cp.Status.LastChecked = &now

	if err := r.Status().Patch(ctx, cp, client.MergeFrom(orig)); err != nil {
		log.Error(err, "failed to patch ControlPlane status")
	}

	// Emit recovery event only when transitioning from Initializing → Ready.
	if prevPhase == controlPlanePhaseInitializing {
		r.Recorder.Eventf(cp, nil, corev1.EventTypeNormal, eventReasonCPFDBReady, eventReasonCPFDBReady, "FDB health check passed; control plane is ready")
	}

	if prevPhase != controlPlanePhaseAvailable {
		log.Info("control plane ready")
	} else {
		log.V(1).Info("control plane still ready")
	}
	return ctrl.Result{RequeueAfter: controlPlaneRequeueInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Every probe stamps status.lastChecked, and an unfiltered watch turns
		// that write into another reconcile, which probes and stamps again. The
		// loop settles only because the second stamp lands in the same second and
		// patches nothing, which costs a second probe of the control plane every
		// interval and reports it twice. The generation does not move on a status
		// write, so this leaves the requeue below as the probe's only clock while
		// an edit to the spec still arrives at once.
		For(&simplyblockv1alpha2.ControlPlane{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("controlplane").
		Complete(r)
}
