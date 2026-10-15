// Scheduling-driven sync replication at the node: deciding, per PVC at stage
// time, whether a volume is managed by a synchronous VolumeReplication and must
// follow its pod, and (later) performing that switchover
// (design-sync-replication-csi-addons.md §6). The detection here is the gate in
// front of the ordinary staging path; the switchover itself lands in a later
// step.
package node

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/kube"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// syncReplicationResolver is the narrow slice of the atlas-lib resolver the node
// plugin needs: the per-PVC lookup of the synchronous VolumeReplication that
// protects a claim. *kube.InformerResolver satisfies it. The site is carried on
// the VolumeReplication itself (its SyncSiteAnnotation), not selected from a
// class, so no class lookup is needed (design §6.2).
type syncReplicationResolver interface {
	SyncVolumeReplicationForClaim(ctx context.Context, claim types.NamespacedName) (kube.VolumeReplicationRef, error)
}

// The VolumeReplication spec.replicationState and status.state values the
// switchover sets and the gate reads (csi-addons: lowercase spec verb, titled
// status).
const (
	replicationStatePrimary   = "primary"
	replicationStateSecondary = "secondary"
	observedStatePrimary      = "Primary"
	observedStateSecondary    = "Secondary"
)

// detectSyncReplication decides, per PVC at stage time, whether vc's volume is
// managed by a synchronous VolumeReplication and must follow its pod (design
// §6.6). It returns (ref, true, nil) for a sync-managed volume, (zero, false,
// nil) for an ordinary one, and a retryable error when the determination cannot
// be made, so a sync volume is never staged on the ordinary path on a guess.
//
// The determination is per PVC, from the informer cache, and trusts a synced
// cache: a not-found read on synced caches is an ordinary volume. It fails
// closed only while the caches have not synced, which is the one window a
// not-found could be a sync volume the cache has not seen yet (design §6.6).
func (ns *Server) detectSyncReplication(
	ctx context.Context, vc map[string]string,
) (kube.VolumeReplicationRef, bool, error) {
	if ns.syncResolver == nil {
		// Sync replication is not configured on this node, so every volume stages
		// as it does today: no lookup, no gate.
		return kube.VolumeReplicationRef{}, false, nil
	}
	if ns.syncCachesSynced == nil || !ns.syncCachesSynced() {
		return kube.VolumeReplicationRef{}, false, status.Error(codes.Unavailable,
			"sync-replication informer caches are not synced yet; retry staging")
	}
	claimNS, claimName := vc[csicommon.CSIStorageNamespaceKey], vc[csicommon.CSIStorageNameKey]
	if claimNS == "" || claimName == "" {
		return kube.VolumeReplicationRef{}, false, status.Error(codes.FailedPrecondition,
			"cannot determine sync-replication membership: the volume carries no PVC identity "+
				"(external-provisioner --extra-create-metadata is required on a sync cluster)")
	}

	ref, err := ns.syncResolver.SyncVolumeReplicationForClaim(ctx,
		types.NamespacedName{Namespace: claimNS, Name: claimName})
	switch {
	case errors.Is(err, errs.ErrNotFound):
		// No synchronous VolumeReplication protects this PVC, so it is an ordinary
		// volume. The synced caches make this answer trustworthy.
		return kube.VolumeReplicationRef{}, false, nil
	case err != nil:
		return kube.VolumeReplicationRef{}, false, status.Error(codes.Unavailable, err.Error())
	default:
		return ref, true, nil
	}
}

// ensureSyncPromoted drives the switchover for a sync-managed volume so it is
// served on this node's site before the ordinary attach runs (design §6.2-§6.4).
// It sets the VolumeReplication's SyncSiteAnnotation to this node's site, as
// primary, and then gates: it returns nil only once the controller reports the
// promote complete, at which point staging falls through to the ordinary attach,
// which connects to the now-led (this) site. Every other outcome is a retryable
// error so the kubelet re-stages. It never blocks: one step per call, no inline
// poll (design §6.4, return-and-retry).
func (ns *Server) ensureSyncPromoted(ctx context.Context, vr kube.VolumeReplicationRef) error {
	if ns.syncNodeSite == nil || ns.syncPatchVR == nil {
		return status.Error(codes.Internal,
			"sync replication is half-configured: the node-site reader or the VolumeReplication patch is unset")
	}
	site, err := ns.syncNodeSite(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "read this node's site: %v", err)
	}

	// A move forces the promote to re-run, the way RamenDR drives a failover: the
	// site rides on the mutable sync-site annotation, but an annotation-only change
	// leaves the VolumeReplication already `primary`/Completed, so the csi-addons
	// controller never re-issues PromoteVolume and the backend's leadership never
	// moves. Driving replicationState primary -> secondary -> primary is a real
	// spec transition the controller acts on, re-issuing DemoteVolume then
	// PromoteVolume -- the latter the backend failover to this site (design §6.3,
	// §6.4). The gate below can only release once that fresh promote completes,
	// because the status it reads is site-agnostic and would otherwise be stale.
	//
	// Step 1 -- begin the move: the volume is promoted (Primary, Completed) on
	// another site. Demote it (replicationState: secondary) and move the annotation
	// to this site. The demote targets this site and is a backend no-op; the switch
	// fences the old site through the failover, not here.
	if vr.Site != site && vr.ObservedState == observedStatePrimary && vr.Completed {
		if err := ns.syncPatchVR(ctx, vr, site, replicationStateSecondary); err != nil {
			return status.Errorf(codes.Unavailable,
				"begin switchover to site %q on %s/%s: %v", site, vr.Namespace, vr.Name, err)
		}
		return status.Errorf(codes.Unavailable,
			"switchover to site %q started on %s/%s; retrying", site, vr.Namespace, vr.Name)
	}

	// Step 2 -- re-promote: we asked for secondary. Once the controller has
	// observed the demote (status Secondary), ask for primary, which it turns into
	// PromoteVolume -> failover?site=<this site>. Waiting for the demote to be
	// observed is what makes secondary -> primary a transition the controller sees,
	// rather than a no-op that collapses back to primary.
	if vr.Site == site && vr.ReplicationState == replicationStateSecondary {
		if vr.ObservedState != observedStateSecondary {
			return status.Errorf(codes.Unavailable,
				"demote before re-promote on site %q of %s/%s is converging; retrying", site, vr.Namespace, vr.Name)
		}
		if err := ns.syncPatchVR(ctx, vr, site, replicationStatePrimary); err != nil {
			return status.Errorf(codes.Unavailable,
				"re-promote to site %q on %s/%s: %v", site, vr.Namespace, vr.Name, err)
		}
		return status.Errorf(codes.Unavailable,
			"re-promote to site %q started on %s/%s; retrying", site, vr.Namespace, vr.Name)
	}

	// First promote (or the annotation/state not yet set to this site): set the
	// sync-site annotation and replicationState: primary. On a fresh
	// VolumeReplication the controller promotes naturally, so there is nothing to
	// re-trigger and no demote step. A single mutable patch, so the class never
	// changes.
	if vr.Site != site || vr.ReplicationState != replicationStatePrimary {
		if err := ns.syncPatchVR(ctx, vr, site, replicationStatePrimary); err != nil {
			return status.Errorf(codes.Unavailable,
				"signal site %q onto VolumeReplication %s/%s: %v", site, vr.Namespace, vr.Name, err)
		}
		return status.Errorf(codes.Unavailable,
			"switchover to site %q requested for %s/%s; retrying", site, vr.Namespace, vr.Name)
	}

	// Promoted and complete on this site: the Primary state with the Completed
	// condition set, reached after the promote above rather than inherited from a
	// prior site (design §6.4, §8).
	if vr.ObservedState == observedStatePrimary && vr.Completed {
		return nil
	}
	return status.Errorf(codes.Unavailable,
		"promote of %s/%s on site %q is converging; retrying", vr.Namespace, vr.Name, site)
}
