// An in-memory control plane for the tests of the state volume, the allowed
// hosts, the coordinator, and the driver controller that runs it: volumes with
// allowed-host lists, call counts, and failures injected per call.

package mdsstatetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
)

// The IDs the fake serves.
const (
	Cluster = "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90"
	Pool    = "11111111-2222-4333-8444-555555555555"
)

// Volume is one volume and the hosts it allows, none meaning every host.
type Volume struct {
	Vol   lvol.Volume
	Hosts []string
}

// API implements the control-plane calls the metadata server's volumes need.
type API struct {
	mu      sync.Mutex
	Volumes map[lvol.VolumeHandle]*Volume
	Pools   map[string]string // name to ID
	next    int

	Creates, Deletes int
	CreateErr        error
	DeleteErr        error
	// HostErr fails a host call for the volume, keyed by handle.
	HostErr map[lvol.VolumeHandle]error
	// AfterCreate runs once the volume exists, before the call returns, so a
	// test can crash the caller between the create and the record.
	AfterCreate func()
	HostCalls   []string
}

func New() *API {
	return &API{Volumes: map[lvol.VolumeHandle]*Volume{}, Pools: map[string]string{}, HostErr: map[lvol.VolumeHandle]error{}}
}

// Add puts a volume in the pool, restricting hosts to the list unless it is empty.
func (f *API) Add(name string, hosts ...string) lvol.VolumeHandle {
	f.next++
	h := lvol.VolumeHandle(fmt.Sprintf("%s:%s:00000000-0000-4000-8000-%012d", Cluster, Pool, f.next))
	f.Volumes[h] = &Volume{Vol: lvol.Volume{ID: h, Name: name, Pool: Pool}, Hosts: hosts}
	return h
}

func (f *API) CreateVolume(_ context.Context, clusterID, poolID string, p controlplane.CreateVolumeParams) (lvol.VolumeHandle, error) {
	f.mu.Lock()
	f.Creates++
	if f.CreateErr != nil {
		f.mu.Unlock()
		return "", f.CreateErr
	}
	f.next++
	h := lvol.VolumeHandle(fmt.Sprintf("%s:%s:00000000-0000-4000-8000-%012d", clusterID, poolID, f.next))
	f.Volumes[h] = &Volume{Vol: lvol.Volume{ID: h, Name: p.Name, Pool: poolID, SizeBytes: p.SizeBytes}}
	after := f.AfterCreate
	f.mu.Unlock()
	if after != nil {
		after()
	}
	return h, nil
}

func (f *API) ListVolumes(_ context.Context, clusterID, poolID string) ([]lvol.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []lvol.Volume
	for h, v := range f.Volumes {
		if hh, _ := lvol.ParseHandle(h); hh.ClusterID == clusterID && hh.PoolRef == poolID {
			out = append(out, v.Vol)
		}
	}
	return out, nil
}

func (f *API) DeleteVolume(_ context.Context, h lvol.VolumeHandle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Deletes++
	if f.DeleteErr != nil {
		return f.DeleteErr
	}
	delete(f.Volumes, h) // an absent volume is a 404, which the real client counts as done
	return nil
}

func (f *API) StoragePoolByName(_ context.Context, _, name string) (controlplane.StoragePool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.Pools[name]
	if !ok {
		return controlplane.StoragePool{}, errors.New("no such pool")
	}
	return controlplane.StoragePool{ID: id, Name: name}, nil
}

func (f *API) EnsureVolumeHost(_ context.Context, h lvol.VolumeHandle, hostNQN string) (controlplane.HostGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.HostCalls = append(f.HostCalls, "ensure "+hostNQN+" "+string(h))
	if err := f.HostErr[h]; err != nil {
		return 0, err
	}
	v, ok := f.Volumes[h]
	if !ok {
		return 0, errors.New("no such volume")
	}
	switch {
	case len(v.Hosts) == 0:
		return controlplane.HostNotNeeded, nil
	case slices.Contains(v.Hosts, hostNQN):
		return controlplane.HostAlreadyAllowed, nil
	}
	v.Hosts = append(v.Hosts, hostNQN)
	return controlplane.HostAdded, nil
}

func (f *API) RemoveVolumeHost(_ context.Context, h lvol.VolumeHandle, hostNQN string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.HostCalls = append(f.HostCalls, "remove "+hostNQN+" "+string(h))
	if err := f.HostErr[h]; err != nil {
		return err
	}
	if v, ok := f.Volumes[h]; ok {
		v.Hosts = slices.DeleteFunc(v.Hosts, func(s string) bool { return s == hostNQN })
	}
	return nil
}

func (f *API) HostsOf(h lvol.VolumeHandle) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.Volumes[h].Hosts)
}
