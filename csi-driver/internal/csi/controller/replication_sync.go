// Sync-replication dispatch for the csi-addons Replication service. A
// VolumeReplicationClass selects the backend per RPC through two parameters
// read from req.GetParameters(): the method (async, the default, or sync) and,
// for sync, the site the switchover targets. This file owns that selection and
// the sync-specific backend calls; the async path stays in replication.go.
//
// Design: design-sync-replication-csi-addons.md §5.
package controller

import (
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	atlascp "github.com/simplyblock/atlas/controlplane"
)

const (
	// methodParam selects the replication backend a class drives. Absent or
	// "async" keeps the snapshot-replication path (design-csi-addons-replication.md).
	// "sync" drives the two-site synchronous backend (sync-replication.md).
	methodParam = "replication.storage.simplyblock.io/method"
	// siteParam is the site a sync switchover targets, passed as ?site= on every
	// sync write route. Required when the method is sync, ignored otherwise.
	siteParam = "replication.storage.simplyblock.io/site"

	methodAsync = "async"
	methodSync  = "sync"
)

// resolveMethod reads the replication method from a class's parameters and, for
// the sync method, the site. An absent method is async, so every existing class
// keeps its behavior. A sync method without a site, or an unrecognized method
// value, is an InvalidArgument the RPC surfaces rather than a silent wrong path
// (design §5.1, §10).
func resolveMethod(params map[string]string) (method, site string, err error) {
	switch params[methodParam] {
	case "", methodAsync:
		return methodAsync, "", nil
	case methodSync:
		site := params[siteParam]
		if site == "" {
			return "", "", errSyncClassMissingSite()
		}
		return methodSync, site, nil
	default:
		return "", "", status.Error(codes.InvalidArgument, fmt.Sprintf(
			"unknown replication method %q (want %q or %q)",
			params[methodParam], methodAsync, methodSync))
	}
}

// errSyncClassMissingSite is the InvalidArgument returned when a sync class
// omits the required site parameter.
func errSyncClassMissingSite() error {
	return status.Error(codes.InvalidArgument, fmt.Sprintf(
		"sync replication class requires the %q parameter", siteParam))
}

// classifySyncError maps a sync route's HTTP status onto the gRPC code the
// csi-addons controller acts on, which is the backend's protocol (design §11).
// The 412 is the single escalation trigger and is the only FailedPrecondition,
// so a 409 gate refusal never drives a forced promote.
func classifySyncError(err error) error {
	if err == nil {
		return nil
	}
	var se *atlascp.SyncStatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusPreconditionFailed:
			// 412: the other site is offline on an unforced promote. The
			// controller re-issues this as a forced promote.
			return status.Error(codes.FailedPrecondition, se.Error())
		case http.StatusBadRequest:
			// 400: a bad or missing site, a misconfigured class.
			return status.Error(codes.InvalidArgument, se.Error())
		default:
			// 409 (in progress or a gate refusal) and 500 (an ANA RPC failure)
			// are retryable and must not be FailedPrecondition, or the controller
			// would wrongly escalate a desynced store to a forced promote.
			return status.Error(codes.Unavailable, se.Error())
		}
	}
	return status.Error(codes.Unavailable, err.Error())
}
