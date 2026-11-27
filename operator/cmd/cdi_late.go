// Starts the StorageProfile controller once KubeVirt's CDI is served, which on
// most clusters happens when OpenShift Virtualization is installed after
// simplyblock.

package main

import (
	"context"
	"time"

	"github.com/go-logr/logr"
)

const (
	// cdiGroupVersion and cdiStorageProfileResource are CDI's StorageProfile,
	// which the StorageProfile controller (internal/controllers/virtualization)
	// watches.
	cdiGroupVersion           = "cdi.kubevirt.io/v1beta1"
	cdiStorageProfileResource = "storageprofiles"

	// cdiPollInterval is how often the operator looks again for CDI's kind when
	// it was not served at start. It doubles after each miss up to
	// ocmPollMaxInterval.
	cdiPollInterval = 30 * time.Second
)

// cdiLateStart registers the StorageProfile controller once CDI is served. A
// watch on a kind the API server does not serve never syncs its cache, which
// stops the whole manager, so the controller waits for the kind rather than the
// operator waiting for a restart. Controllers added to a running manager start
// at once.
type cdiLateStart struct {
	log      logr.Logger
	disc     serverResourcesGetter
	register func() error
	after    func(d time.Duration) <-chan time.Time
}

// Start implements manager.Runnable.
func (o *cdiLateStart) Start(ctx context.Context) error {
	after := o.after
	if after == nil {
		after = time.After
	}
	if !waitForResource(ctx, o.log, o.disc, cdiGroupVersion, cdiStorageProfileResource,
		cdiPollInterval, ocmPollMaxInterval, after) {
		return nil
	}
	o.log.Info("CDI's StorageProfile is now served; starting the StorageProfile controller",
		"groupVersion", cdiGroupVersion, "resource", cdiStorageProfileResource)
	return o.register()
}
