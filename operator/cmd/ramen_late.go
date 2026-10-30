package main

import (
	"context"
	"time"

	"github.com/go-logr/logr"
)

const (
	// ramenGroupVersion and ramenVRGResource are Ramen's VolumeReplicationGroup,
	// which the VRG follower (internal/controllers/vrgfollow) watches.
	ramenGroupVersion = "ramendr.openshift.io/v1alpha1"
	ramenVRGResource  = "volumereplicationgroups"

	// ramenPollInterval is how often the operator looks again for Ramen's kind
	// when it was not served at start. It doubles after each miss up to
	// ocmPollMaxInterval. Ramen is commonly installed minutes after the operator.
	ramenPollInterval = 30 * time.Second
)

// ramenLateStart registers the VRG follower once Ramen's VolumeReplicationGroup
// is served. A stretched site's operator is installed before Ramen, and a watch
// on a kind the API server does not serve never syncs its cache, which stops the
// whole manager. So the follower waits for the kind rather than the operator
// waiting for a restart. Controllers added to a running manager start at once.
type ramenLateStart struct {
	log      logr.Logger
	disc     serverResourcesGetter
	register func() error
	after    func(time.Duration) <-chan time.Time
}

// Start implements manager.Runnable.
func (o *ramenLateStart) Start(ctx context.Context) error {
	after := o.after
	if after == nil {
		after = time.After
	}
	if !waitForResource(ctx, o.log, o.disc, ramenGroupVersion, ramenVRGResource,
		ramenPollInterval, ocmPollMaxInterval, after) {
		return nil
	}
	o.log.Info("Ramen's VolumeReplicationGroup is now served; starting the VRG follower",
		"groupVersion", ramenGroupVersion, "resource", ramenVRGResource)
	return o.register()
}
