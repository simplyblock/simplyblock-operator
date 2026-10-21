// Authorizing every pod's host NQN on the volumes the guests connect
// (design-pnfs-mds-standby.md P0-2).

package mdsstate

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/simplyblock-operator/internal/mdsstate/mdsstatetest"
)

func hostOf(t *testing.T, ordinal int32) string {
	t.Helper()
	h, err := HostNQN(testUID, ordinal)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestSyncHostsAllowsEveryOrdinalOnVolumesThatRestrictHosts(t *testing.T) {
	api := mdsstatetest.New()
	state := api.Add("state", "nqn.client-a")
	export := api.Add("export", "nqn.client-a", "nqn.client-b")
	sts := testSTS(nil)
	hs := &HostSync{Client: kubeFor(t, sts), API: api}

	res, err := hs.Sync(context.Background(), sts, []lvol.VolumeHandle{state, export}, 3)
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("Sync = %+v, %v", res, err)
	}
	for _, v := range []lvol.VolumeHandle{state, export} {
		for o := int32(0); o < 3; o++ {
			if !slices.Contains(api.HostsOf(v), hostOf(t, o)) {
				t.Errorf("volume %s lacks the host of ordinal %d: %v", v, o, api.HostsOf(v))
			}
		}
	}
	if !slices.Contains(api.HostsOf(export), "nqn.client-b") {
		t.Error("a host that was already allowed was dropped")
	}
}

// A volume that lists no hosts allows every host: adding one would lock every
// other client out at the control plane's next re-apply.
func TestSyncHostsLeavesAVolumeWithoutAllowedHostsAlone(t *testing.T) {
	api := mdsstatetest.New()
	open := api.Add("open")
	sts := testSTS(nil)
	hs := &HostSync{Client: kubeFor(t, sts), API: api}

	res, err := hs.Sync(context.Background(), sts, []lvol.VolumeHandle{open}, 2)
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("Sync = %+v, %v", res, err)
	}
	if got := api.HostsOf(open); len(got) != 0 {
		t.Errorf("hosts = %v, want the volume left open", got)
	}
}

func TestSyncHostsIsIdempotent(t *testing.T) {
	api := mdsstatetest.New()
	v := api.Add("export", "nqn.client-a")
	sts := testSTS(nil)
	c := kubeFor(t, sts)
	hs := &HostSync{Client: c, API: api}

	for range 3 {
		if _, err := hs.Sync(context.Background(), reload(t, c, sts), []lvol.VolumeHandle{v}, 2); err != nil {
			t.Fatal(err)
		}
	}
	if got := api.HostsOf(v); len(got) != 3 {
		t.Errorf("hosts = %v, want the client and two pods once each", got)
	}
}

// One volume that cannot be updated is reported and retried, and the volumes
// that can are done: a promotion of those does not wait.
func TestSyncHostsReportsAVolumeThatFailsAndFinishesTheRest(t *testing.T) {
	api := mdsstatetest.New()
	bad := api.Add("bad", "nqn.client-a")
	good := api.Add("good", "nqn.client-a")
	api.HostErr[bad] = errors.New("503")
	sts := testSTS(nil)
	hs := &HostSync{Client: kubeFor(t, sts), API: api}

	res, err := hs.Sync(context.Background(), sts, []lvol.VolumeHandle{bad, good}, 2)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[bad] == nil {
		t.Errorf("failed = %v, want only the bad volume", res.Failed)
	}
	if !slices.Contains(api.HostsOf(good), hostOf(t, 1)) {
		t.Error("the good volume was not updated after the bad one failed")
	}
	if res.Allowed(bad) || !res.Allowed(good) {
		t.Errorf("Allowed(bad) = %v, Allowed(good) = %v", res.Allowed(bad), res.Allowed(good))
	}
}

// Scale-down removes the hosts of the ordinals that are gone, once, and a
// failed removal is retried.
func TestSyncHostsRemovesTheHostsOfRemovedOrdinals(t *testing.T) {
	api := mdsstatetest.New()
	v := api.Add("export", "nqn.client-a")
	sts := testSTS(nil)
	c := kubeFor(t, sts)
	hs := &HostSync{Client: c, API: api}
	ctx := context.Background()

	if _, err := hs.Sync(ctx, sts, []lvol.VolumeHandle{v}, 3); err != nil {
		t.Fatal(err)
	}
	if got := reload(t, c, sts).Annotations[HostOrdinalsAnnotation]; got != "3" {
		t.Fatalf("recorded ordinals = %q, want 3", got)
	}

	api.HostErr[v] = errors.New("503")
	if res, _ := hs.Sync(ctx, reload(t, c, sts), []lvol.VolumeHandle{v}, 1); len(res.Failed) != 1 {
		t.Fatalf("a failed removal was not reported: %+v", res)
	}
	if got := reload(t, c, sts).Annotations[HostOrdinalsAnnotation]; got != "3" {
		t.Errorf("recorded ordinals = %q after a failed removal, want 3 kept for the retry", got)
	}

	delete(api.HostErr, v)
	if res, err := hs.Sync(ctx, reload(t, c, sts), []lvol.VolumeHandle{v}, 1); err != nil || len(res.Failed) != 0 {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	hosts := api.HostsOf(v)
	if !slices.Contains(hosts, hostOf(t, 0)) || slices.Contains(hosts, hostOf(t, 1)) || slices.Contains(hosts, hostOf(t, 2)) {
		t.Errorf("hosts = %v, want ordinal 0 kept and 1 and 2 removed", hosts)
	}
	if got := reload(t, c, sts).Annotations[HostOrdinalsAnnotation]; got != strconv.Itoa(1) {
		t.Errorf("recorded ordinals = %q, want 1", got)
	}

	// Settled: nothing further is removed.
	before := len(api.HostCalls)
	if _, err := hs.Sync(ctx, reload(t, c, sts), []lvol.VolumeHandle{v}, 1); err != nil {
		t.Fatal(err)
	}
	for _, call := range api.HostCalls[before:] {
		if len(call) >= 6 && call[:6] == "remove" {
			t.Errorf("a settled sync made %q", call)
		}
	}
}
