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

package spdk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/spdk/spdk-csi/pkg/util"
)

// NodeGetVolumeStats touches none of nodeServer's fields, so the zero value is
// a valid receiver for these tests -- no mounter/kubeClient/driver setup needed.

const testVolumeID = "vol"

func TestNodeGetVolumeStats_MissingArgs(t *testing.T) {
	ns := &nodeServer{}
	ctx := context.Background()

	req := &csi.NodeGetVolumeStatsRequest{VolumePath: "/tmp"}
	if _, err := ns.NodeGetVolumeStats(ctx, req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing volume_id, got %v", err)
	}
	req = &csi.NodeGetVolumeStatsRequest{VolumeId: testVolumeID}
	if _, err := ns.NodeGetVolumeStats(ctx, req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing volume_path, got %v", err)
	}
}

func TestNodeGetVolumeStats_VolumePathMissing(t *testing.T) {
	ns := &nodeServer{}

	_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   testVolumeID,
		VolumePath: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestNodeGetVolumeStats_HealthyDirectoryReportsNormal(t *testing.T) {
	ns := &nodeServer{}

	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   testVolumeID,
		VolumePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cond := resp.GetVolumeCondition(); cond == nil || cond.GetAbnormal() {
		t.Fatalf("expected Abnormal=false for a healthy mount, got %+v", cond)
	}
}

// TestNodeGetVolumeStats_DisconnectedDeviceReportsAbnormal is the core regression
// test for the bug this fix addresses: a cached stat/statfs on the staging
// directory can still succeed even after the backing NVMe-oF device is gone, so
// NodeGetVolumeStats must cross-check the stashed devicePath independently.
func TestNodeGetVolumeStats_DisconnectedDeviceReportsAbnormal(t *testing.T) {
	ns := &nodeServer{}
	dir := t.TempDir()

	stagingParentPath := filepath.Join(dir, "staging")
	if err := os.MkdirAll(stagingParentPath, 0o755); err != nil {
		t.Fatalf("mkdir staging parent: %v", err)
	}
	volumePath := filepath.Join(dir, "mount")
	if err := os.MkdirAll(volumePath, 0o755); err != nil {
		t.Fatalf("mkdir volume path: %v", err)
	}

	missingDevice := filepath.Join(dir, "nvme-disconnected")
	if err := util.StashVolumeContext(map[string]string{"devicePath": missingDevice}, stagingParentPath); err != nil {
		t.Fatalf("stash volume context: %v", err)
	}

	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:          testVolumeID,
		VolumePath:        volumePath,
		StagingTargetPath: stagingParentPath,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cond := resp.GetVolumeCondition()
	if cond == nil || !cond.GetAbnormal() {
		t.Fatalf("expected Abnormal=true when the stashed device is gone, got %+v", cond)
	}
	if !strings.Contains(cond.GetMessage(), missingDevice) {
		t.Fatalf("expected message to reference the missing device path, got %q", cond.GetMessage())
	}
}

func TestNodeGetVolumeStats_ConnectedDeviceStillReportsNormal(t *testing.T) {
	ns := &nodeServer{}
	dir := t.TempDir()

	stagingParentPath := filepath.Join(dir, "staging")
	if err := os.MkdirAll(stagingParentPath, 0o755); err != nil {
		t.Fatalf("mkdir staging parent: %v", err)
	}
	volumePath := filepath.Join(dir, "mount")
	if err := os.MkdirAll(volumePath, 0o755); err != nil {
		t.Fatalf("mkdir volume path: %v", err)
	}

	existingDevice := filepath.Join(dir, "nvme-connected")
	if err := os.WriteFile(existingDevice, []byte("x"), 0o600); err != nil {
		t.Fatalf("create fake device path: %v", err)
	}
	if err := util.StashVolumeContext(map[string]string{"devicePath": existingDevice}, stagingParentPath); err != nil {
		t.Fatalf("stash volume context: %v", err)
	}

	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:          testVolumeID,
		VolumePath:        volumePath,
		StagingTargetPath: stagingParentPath,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cond := resp.GetVolumeCondition(); cond.GetAbnormal() {
		t.Fatalf("expected Abnormal=false when the stashed device still exists, got %+v", cond)
	}
}

func TestNodeGetVolumeStats_NoStashSkipsDeviceCheck(t *testing.T) {
	ns := &nodeServer{}
	dir := t.TempDir()

	// No stash written at stagingParentPath (e.g. kubelet didn't pass
	// staging_target_path, or nothing has been staged yet at this path).
	// The device check must be skipped, not treated as abnormal.
	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:          testVolumeID,
		VolumePath:        dir,
		StagingTargetPath: filepath.Join(dir, "no-such-staging-path"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cond := resp.GetVolumeCondition(); cond.GetAbnormal() {
		t.Fatalf("expected Abnormal=false when no stash exists, got %+v", cond)
	}
}

// TestNodeGetVolumeStats_BlockSizeErrorReportsAbnormal exercises the non-directory
// (raw block volume) path: a regular file is not a block device, so the
// BLKGETSIZE64 ioctl legitimately fails, which must now surface as
// Abnormal: true instead of an opaque gRPC Internal error.
func TestNodeGetVolumeStats_BlockSizeErrorReportsAbnormal(t *testing.T) {
	ns := &nodeServer{}
	dir := t.TempDir()

	volumePath := filepath.Join(dir, "block-target")
	if err := os.WriteFile(volumePath, []byte("not a real block device"), 0o600); err != nil {
		t.Fatalf("create fake block target: %v", err)
	}

	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   testVolumeID,
		VolumePath: volumePath,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cond := resp.GetVolumeCondition(); cond == nil || !cond.GetAbnormal() {
		t.Fatalf("expected Abnormal=true for a non-block-device volume path, got %+v", cond)
	}
}
