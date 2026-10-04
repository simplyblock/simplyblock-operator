package main

import (
	"context"
	"time"

	"github.com/go-logr/logr"
)

const (
	// ocmPollInterval is how often the operator looks again for the OCM
	// ManifestWork resource when it was not served at start; it doubles after
	// each miss up to ocmPollMaxInterval.
	ocmPollInterval    = 60 * time.Second
	ocmPollMaxInterval = 10 * time.Minute
)

// waitForResource polls discovery until the API server serves resource in
// groupVersion, and reports true; false when ctx ends first. Discovery errors
// are logged and polled through: they are as likely to be a passing API server
// hiccup as a real absence. after is time.After outside of tests.
func waitForResource(ctx context.Context, log logr.Logger, disc serverResourcesGetter, groupVersion, resource string,
	interval, maxInterval time.Duration, after func(time.Duration) <-chan time.Time) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-after(interval):
		}
		served, err := serverHasResource(disc, groupVersion, resource)
		if err != nil {
			log.Info("could not check for the OCM ManifestWork resource; trying again",
				"groupVersion", groupVersion, "resource", resource, "error", err.Error())
		} else if served {
			return true
		}
		if interval *= 2; interval > maxInterval {
			interval = maxInterval
		}
	}
}

// ocmLateStart registers the hub-only controllers once OCM's ManifestWork is
// served. The operator is commonly installed before OCM (the DR stack brings
// it); checking only at process start left every TestFailover without a
// reconciler until the operator happened to restart (2026-10-04). Controllers
// added to a running manager are started by controller-runtime right away
// (manager runnableGroup.Add), and neither controller registers field indexes,
// so no restart of the process is needed.
type ocmLateStart struct {
	log      logr.Logger
	disc     serverResourcesGetter
	register func() error
	after    func(time.Duration) <-chan time.Time
}

// Start implements manager.Runnable.
func (o *ocmLateStart) Start(ctx context.Context) error {
	after := o.after
	if after == nil {
		after = time.After
	}
	if !waitForResource(ctx, o.log, o.disc, ocmWorkGroupVersion, ocmManifestWorkResource,
		ocmPollInterval, ocmPollMaxInterval, after) {
		return nil
	}
	o.log.Info("OCM ManifestWork resource is now served; starting the TestFailover and "+
		"StorageSiteDeployment controllers", "groupVersion", ocmWorkGroupVersion, "resource", ocmManifestWorkResource)
	return o.register()
}
