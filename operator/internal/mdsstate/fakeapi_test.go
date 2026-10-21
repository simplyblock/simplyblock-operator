// An in-memory control plane for the state volume, allowed-hosts, and
// coordinator tests: volumes with allowed-host lists, call counts, and failures
// injected per call.

package mdsstate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
)

const (
	testCluster = "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90"
	testPool    = "11111111-2222-4333-8444-555555555555"
)

type fakeVolume struct {
	vol   lvol.Volume
	hosts []string
}

type fakeAPI struct {
	mu      sync.Mutex
	volumes map[lvol.VolumeHandle]*fakeVolume
	pools   map[string]string // name to ID
	next    int

	creates, deletes int
	createErr        error
	deleteErr        error
	// hostErr fails a host call for the volume, keyed by handle.
	hostErr map[lvol.VolumeHandle]error
	// afterCreate runs once the volume exists, before the call returns, so a
	// test can crash the caller between the create and the record.
	afterCreate func()
	hostCalls   []string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{volumes: map[lvol.VolumeHandle]*fakeVolume{}, pools: map[string]string{}, hostErr: map[lvol.VolumeHandle]error{}}
}

// add puts a volume in the pool, restricting hosts to the list unless it is empty.
func (f *fakeAPI) add(name string, hosts ...string) lvol.VolumeHandle {
	f.next++
	h := lvol.VolumeHandle(fmt.Sprintf("%s:%s:00000000-0000-4000-8000-%012d", testCluster, testPool, f.next))
	f.volumes[h] = &fakeVolume{vol: lvol.Volume{ID: h, Name: name, Pool: testPool}, hosts: hosts}
	return h
}

func (f *fakeAPI) CreateVolume(_ context.Context, clusterID, poolID string, p controlplane.CreateVolumeParams) (lvol.VolumeHandle, error) {
	f.mu.Lock()
	f.creates++
	if f.createErr != nil {
		f.mu.Unlock()
		return "", f.createErr
	}
	f.next++
	h := lvol.VolumeHandle(fmt.Sprintf("%s:%s:00000000-0000-4000-8000-%012d", clusterID, poolID, f.next))
	f.volumes[h] = &fakeVolume{vol: lvol.Volume{ID: h, Name: p.Name, Pool: poolID, SizeBytes: p.SizeBytes}}
	after := f.afterCreate
	f.mu.Unlock()
	if after != nil {
		after()
	}
	return h, nil
}

func (f *fakeAPI) ListVolumes(_ context.Context, clusterID, poolID string) ([]lvol.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []lvol.Volume
	for h, v := range f.volumes {
		if hh, _ := lvol.ParseHandle(h); hh.ClusterID == clusterID && hh.PoolRef == poolID {
			out = append(out, v.vol)
		}
	}
	return out, nil
}

func (f *fakeAPI) DeleteVolume(_ context.Context, h lvol.VolumeHandle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.volumes, h) // an absent volume is a 404, which the real client counts as done
	return nil
}

func (f *fakeAPI) StoragePoolByName(_ context.Context, _, name string) (controlplane.StoragePool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.pools[name]
	if !ok {
		return controlplane.StoragePool{}, errors.New("no such pool")
	}
	return controlplane.StoragePool{ID: id, Name: name}, nil
}

func (f *fakeAPI) EnsureVolumeHost(_ context.Context, h lvol.VolumeHandle, hostNQN string) (controlplane.HostGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostCalls = append(f.hostCalls, "ensure "+hostNQN+" "+string(h))
	if err := f.hostErr[h]; err != nil {
		return 0, err
	}
	v, ok := f.volumes[h]
	if !ok {
		return 0, errors.New("no such volume")
	}
	switch {
	case len(v.hosts) == 0:
		return controlplane.HostNotNeeded, nil
	case slices.Contains(v.hosts, hostNQN):
		return controlplane.HostAlreadyAllowed, nil
	}
	v.hosts = append(v.hosts, hostNQN)
	return controlplane.HostAdded, nil
}

func (f *fakeAPI) RemoveVolumeHost(_ context.Context, h lvol.VolumeHandle, hostNQN string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostCalls = append(f.hostCalls, "remove "+hostNQN+" "+string(h))
	if err := f.hostErr[h]; err != nil {
		return err
	}
	if v, ok := f.volumes[h]; ok {
		v.hosts = slices.DeleteFunc(v.hosts, func(s string) bool { return s == hostNQN })
	}
	return nil
}

func (f *fakeAPI) hostsOf(h lvol.VolumeHandle) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.volumes[h].hosts)
}
