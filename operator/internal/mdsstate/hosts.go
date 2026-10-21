// Authorizing every pod's host NQN on the volumes the guests connect
// themselves: the state volume and each export volume of the storage cluster
// (design-pnfs-mds-standby.md P0-2).
//
// Each ordinal is its own host at the target, so a standby that was never added
// to a volume's allowed hosts cannot connect it at promotion. A volume that
// lists no allowed hosts allows every host and is left alone, because adding a
// host to it would restrict it to that host at the control plane's next
// re-apply.

package mdsstate

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
)

// HostSync keeps the allowed hosts of a storage cluster's volumes in step with
// the StatefulSet's ordinals.
type HostSync struct {
	Client client.Client
	API    VolumeAPI
}

// HostResult is what one pass could not do, per volume.
type HostResult struct {
	// Failed holds the volumes whose hosts could not be updated, with the error.
	Failed map[lvol.VolumeHandle]error
}

// Allowed reports whether every ordinal's host is allowed on the volume, so a
// promotion that connects it can go ahead. A volume this pass did not fail is
// allowed, and one it did fail does not hold up any other volume.
func (r HostResult) Allowed(h lvol.VolumeHandle) bool {
	_, failed := r.Failed[h]
	return !failed
}

// Sync allows the host NQNs of ordinals [0, ordinals) on every volume that
// restricts hosts, then removes those of the ordinals a scale-down took away.
// It returns an error only for a failure outside any volume. A volume that
// cannot be updated is in the result, and the other volumes are still done.
//
// The StatefulSet records how many ordinals the volumes allow, so the ordinals
// to remove are known after the StatefulSet has already shrunk. The record
// follows the pass only when nothing failed.
func (s *HostSync) Sync(
	ctx context.Context, sts *appsv1.StatefulSet, volumes []lvol.VolumeHandle, ordinals int32,
) (HostResult, error) {
	recorded := ordinals
	if v, err := strconv.Atoi(sts.Annotations[HostOrdinalsAnnotation]); err == nil && int32(v) > recorded {
		recorded = int32(v)
	}
	allow, err := hostNQNs(sts, 0, ordinals)
	if err != nil {
		return HostResult{}, err
	}
	drop, err := hostNQNs(sts, ordinals, recorded)
	if err != nil {
		return HostResult{}, err
	}

	res := HostResult{Failed: map[lvol.VolumeHandle]error{}}
	for _, h := range volumes {
		if err := s.syncVolume(ctx, h, allow, drop); err != nil {
			res.Failed[h] = err
		}
	}

	settled := ordinals
	if len(res.Failed) > 0 {
		settled = recorded
	}
	if err := s.record(ctx, sts, settled); err != nil {
		return res, err
	}
	return res, nil
}

func (s *HostSync) syncVolume(ctx context.Context, h lvol.VolumeHandle, allow, drop []string) error {
	for _, nqn := range allow {
		grant, err := s.API.EnsureVolumeHost(ctx, h, nqn)
		if err != nil {
			return err
		}
		// Every host is allowed on a volume that lists none, so the rest of
		// the list is no more needed than the first.
		if grant == controlplane.HostNotNeeded {
			return nil
		}
	}
	for _, nqn := range drop {
		if err := s.API.RemoveVolumeHost(ctx, h, nqn); err != nil {
			return err
		}
	}
	return nil
}

// record writes the settled ordinal count when it changed.
func (s *HostSync) record(ctx context.Context, sts *appsv1.StatefulSet, n int32) error {
	want := strconv.Itoa(int(n))
	if sts.Annotations[HostOrdinalsAnnotation] == want {
		return nil
	}
	patch := client.MergeFrom(sts.DeepCopy())
	if sts.Annotations == nil {
		sts.Annotations = map[string]string{}
	}
	sts.Annotations[HostOrdinalsAnnotation] = want
	if err := s.Client.Patch(ctx, sts, patch); err != nil {
		return fmt.Errorf("recording the allowed ordinals on %s: %w", sts.Name, err)
	}
	return nil
}

// hostNQNs are the host NQNs of ordinals [from, to).
func hostNQNs(sts *appsv1.StatefulSet, from, to int32) ([]string, error) {
	var out []string
	for o := from; o < to; o++ {
		nqn, err := HostNQN(sts.UID, o)
		if err != nil {
			return nil, err
		}
		out = append(out, nqn)
	}
	return out, nil
}
