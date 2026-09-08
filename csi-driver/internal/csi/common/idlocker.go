package csicommon

import (
	"sync"
)

// VolumeLocks simple locks that can be acquired by volumeID
type VolumeLocks struct {
	mutexes sync.Map
}

// NewVolumeLocks returns new VolumeLocks.
func NewVolumeLocks() *VolumeLocks {
	return &VolumeLocks{}
}

// Lock obtain the lock corresponding to the volumeID
func (vl *VolumeLocks) Lock(volumeID string) func() {
	value, _ := vl.mutexes.LoadOrStore(volumeID, &sync.Mutex{})
	mtx, _ := value.(*sync.Mutex) //nolint:errcheck // will not fail to convert
	mtx.Lock()
	return func() { mtx.Unlock() }
}
