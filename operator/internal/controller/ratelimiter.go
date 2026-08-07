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
	"time"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// defaultControllerOptions returns controller.Options with an exponential
// backoff rate limiter (base 5 s, max 5 min).
//
// The rate limiter governs how quickly a controller re-queues an item after
// returning a non-nil error (ctrl.Result{}, err). Successive failures for the
// same object back off to 10 s, 20 s, 40 s … up to 5 min, then reset once the
// object reconciles successfully. This prevents thundering-herd retry storms
// when the backend API is degraded.
//
// Callers that already need custom options (e.g. MaxConcurrentReconciles)
// should embed the rate limiter directly:
//
//	controller.Options{MaxConcurrentReconciles: 1, RateLimiter: defaultRateLimiter()}
func defaultControllerOptions() controller.Options {
	return controller.Options{RateLimiter: defaultRateLimiter()}
}

func defaultRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
		5*time.Second,
		5*time.Minute,
	)
}
