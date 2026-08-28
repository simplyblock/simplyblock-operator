/*
Copyright (c) Arm Limited and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package util's VDO wiring. The actual VDO stack lifecycle (assembling,
// activating, deactivating, removing, and growing a per-volume LVM/dm-vdo
// stack) lives in github.com/simplyblock/atlas/lvm/vdo, a node-level
// primitive with no CSI or Kubernetes awareness. This file is the thin CSI
// side: one shared lvm.Manager, and one function per NodeStageVolume/
// NodeUnstageVolume/NodeExpandVolume concern.
package util

import (
	"context"

	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/lvm/vdo"
)

// lvmManager runs every LVM/dm-vdo command this file needs, scoped to a
// device and content-identity-aware where it matters. See
// github.com/simplyblock/atlas/lvm's package doc comment for why: a
// simplyblock NVMe-oF HA volume's two redundant local device nodes present
// byte-identical content, which an unscoped, name-based LVM lookup cannot
// tell apart.
var lvmManager = lvm.NewManager()

// CreateOrAttachVDO idempotently ensures a VDO-backed logical volume exists
// on top of devicePath, named after lvolID, and returns the resulting device
// path to format/mount.
func CreateOrAttachVDO(
	ctx context.Context, devicePath, lvolID string, compression, deduplication bool,
) (string, error) {
	return vdo.CreateOrAttach(ctx, lvmManager, devicePath, lvolID, compression, deduplication)
}

// ResolveClonedVDO handles the case where devicePath is a block-level CoW
// clone/snapshot restore of another VDO-formatted volume. Must run before
// CreateOrAttachVDO whenever the volume was provisioned from a
// VolumeContentSource.
func ResolveClonedVDO(ctx context.Context, devicePath, lvolID string) error {
	return vdo.ResolveClone(ctx, lvmManager, devicePath, lvolID)
}

// DeactivateVDO deactivates (but does not destroy) the VG/LV stack for
// lvolID. The correct, non-destructive counterpart to a plain NVMe-oF
// disconnect: call this from NodeUnstageVolume, never RemoveVDO.
func DeactivateVDO(ctx context.Context, lvolID string) error {
	return vdo.Deactivate(ctx, lvmManager, lvolID)
}

// RemoveVDO deactivates and removes the VG/LV stack for lvolID, destroying
// its data. Only appropriate when the underlying volume itself is actually
// being removed, or to clean up a stale/orphaned stack. Never call this from
// a routine NodeUnstageVolume.
func RemoveVDO(ctx context.Context, lvolID string) error {
	return vdo.Remove(ctx, lvmManager, lvolID)
}

// GrowVDO grows the VDO stack for lvolID to consume devicePath's newly
// available physical space, and returns its device path.
func GrowVDO(ctx context.Context, devicePath, lvolID string) (string, error) {
	return vdo.Grow(ctx, lvmManager, devicePath, lvolID)
}

// SetVDOFeatures toggles compression/deduplication on an existing, active
// VDO volume without recreating it. Not wired into any live update path in
// v1, included because the underlying mechanism exists and is needed if a
// future StorageClass/VolumeAttributesClass update path is added.
func SetVDOFeatures(ctx context.Context, lvolID string, compression, deduplication bool) error {
	return vdo.SetFeatures(ctx, lvmManager, lvolID, compression, deduplication)
}
