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
	"fmt"
	"strconv"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func makeSnapshots(n int) []*csi.ListSnapshotsResponse_Entry {
	entries := make([]*csi.ListSnapshotsResponse_Entry, n)
	for i := range entries {
		entries[i] = &csi.ListSnapshotsResponse_Entry{
			Snapshot: &csi.Snapshot{SnapshotId: fmt.Sprintf("snap-%d", i)},
		}
	}
	return entries
}

// collectAllPages walks all pages and returns the collected entries plus the page
// count. It fails the test if any call returns an error.
func collectAllPages(
	t *testing.T,
	all []*csi.ListSnapshotsResponse_Entry,
	pageSize int,
) ([]*csi.ListSnapshotsResponse_Entry, int) {
	t.Helper()
	var page []*csi.ListSnapshotsResponse_Entry
	token := ""
	pageCount := 0
	for {
		p, next, err := paginateSnapshots(all, token, pageSize)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		page = append(page, p...)
		pageCount++
		if next == "" {
			break
		}
		token = next
	}
	return page, pageCount
}

func TestPaginateSnapshots(t *testing.T) {
	const pageSize = 5

	tests := []struct {
		name              string
		totalSnapshots    int
		expectedPageCount int
	}{
		{"0 entries", 0, 1},
		{"1 entry", 1, 1},
		{"4 entries", 4, 1},
		{"5 entries", 5, 1},
		{"6 entries", 6, 2},
		{"10 entries", 10, 2},
		{"14 entries", 14, 3},
		{"15 entries", 15, 3},
		{"16 entries", 16, 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			all := makeSnapshots(tc.totalSnapshots)
			pages, pageCount := collectAllPages(t, all, pageSize)

			if len(pages) != tc.totalSnapshots {
				t.Errorf("total entries: got %d, want %d", len(pages), tc.totalSnapshots)
			}
			if pageCount != tc.expectedPageCount {
				t.Errorf("page count: got %d, want %d", pageCount, tc.expectedPageCount)
			}
			for i, entry := range pages {
				want := fmt.Sprintf("snap-%d", i)
				if got := entry.Snapshot.SnapshotId; got != want {
					t.Errorf("entry[%d]: got %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestPaginateSnapshots_NoPageSize(t *testing.T) {
	all := makeSnapshots(10)
	page, next, err := paginateSnapshots(all, "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page) != 10 {
		t.Errorf("got %d entries, want 10", len(page))
	}
	if next != "" {
		t.Errorf("got nextToken %q, want empty", next)
	}
}

func TestPaginateSnapshots_TokenBeyondEnd(t *testing.T) {
	all := makeSnapshots(3)
	page, next, err := paginateSnapshots(all, strconv.Itoa(100), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page) != 0 {
		t.Errorf("got %d entries, want 0", len(page))
	}
	if next != "" {
		t.Errorf("got nextToken %q, want empty", next)
	}
}

func TestPaginateSnapshots_InvalidToken(t *testing.T) {
	all := makeSnapshots(5)
	for _, bad := range []string{"abc", "-1", "1.5", ""} {
		if bad == "" {
			continue // empty token is valid (first page)
		}
		_, _, err := paginateSnapshots(all, bad, 2)
		if err == nil {
			t.Errorf("token %q: expected error, got nil", bad)
		}
	}
}

func topologyWithSegments(segments map[string]string) *csi.Topology {
	return &csi.Topology{Segments: segments}
}

func TestCoLocatedHostID(t *testing.T) {
	const clusterID = "cluster-a-uuid"
	const uuid = "20686642-a53d-41e9-b1db-99c1e450bd31"

	t.Run("nil requirements", func(t *testing.T) {
		if got := coLocatedHostID(nil, clusterID); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("no matching segment", func(t *testing.T) {
		req := &csi.TopologyRequirement{
			Preferred: []*csi.Topology{topologyWithSegments(map[string]string{
				topologyKeyZoneStable: "us-east-1a",
			})},
		}
		if got := coLocatedHostID(req, clusterID); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("preferred segment matches", func(t *testing.T) {
		req := &csi.TopologyRequirement{
			Preferred: []*csi.Topology{topologyWithSegments(map[string]string{
				topologyKeyZoneStable:                   "us-east-1a",
				topologyKeyStorageNodeUUIDPrefix + uuid: clusterID + ".0",
			})},
		}
		if got := coLocatedHostID(req, clusterID); got != uuid {
			t.Errorf("got %q, want %q", got, uuid)
		}
	})

	t.Run("falls back to requisite when preferred has no match", func(t *testing.T) {
		req := &csi.TopologyRequirement{
			Preferred: []*csi.Topology{topologyWithSegments(map[string]string{
				topologyKeyZoneStable: "us-east-1a",
			})},
			Requisite: []*csi.Topology{topologyWithSegments(map[string]string{
				topologyKeyStorageNodeUUIDPrefix + uuid: clusterID + ".0",
			})},
		}
		if got := coLocatedHostID(req, clusterID); got != uuid {
			t.Errorf("got %q, want %q", got, uuid)
		}
	})

	t.Run("preferred takes precedence over requisite", func(t *testing.T) {
		const otherUUID = "429448c5-83c0-447d-9546-dd40ac2b20c1"
		req := &csi.TopologyRequirement{
			Preferred: []*csi.Topology{topologyWithSegments(map[string]string{
				topologyKeyStorageNodeUUIDPrefix + uuid: clusterID + ".0",
			})},
			Requisite: []*csi.Topology{topologyWithSegments(map[string]string{
				topologyKeyStorageNodeUUIDPrefix + otherUUID: clusterID + ".0",
			})},
		}
		if got := coLocatedHostID(req, clusterID); got != uuid {
			t.Errorf("got %q, want %q (preferred should win)", got, uuid)
		}
	})

	t.Run("segment from a different cluster is skipped", func(t *testing.T) {
		req := &csi.TopologyRequirement{
			Preferred: []*csi.Topology{topologyWithSegments(map[string]string{
				topologyKeyStorageNodeUUIDPrefix + uuid: "some-other-cluster-uuid.0",
			})},
		}
		if got := coLocatedHostID(req, clusterID); got != "" {
			t.Errorf("got %q, want empty (cluster mismatch)", got)
		}
	})

	t.Run("multiple sockets on one worker: lowest ordinal wins", func(t *testing.T) {
		const socket0UUID = "11111111-1111-1111-1111-111111111111"
		const socket1UUID = "22222222-2222-2222-2222-222222222222"
		req := &csi.TopologyRequirement{
			Preferred: []*csi.Topology{topologyWithSegments(map[string]string{
				topologyKeyStorageNodeUUIDPrefix + socket1UUID: clusterID + ".1",
				topologyKeyStorageNodeUUIDPrefix + socket0UUID: clusterID + ".0",
			})},
		}
		// Run repeatedly — the original prototype picked via unordered map
		// iteration, so a flaky result here would indicate a regression.
		for i := 0; i < 20; i++ {
			if got := coLocatedHostID(req, clusterID); got != socket0UUID {
				t.Fatalf("got %q, want %q (lowest ordinal)", got, socket0UUID)
			}
		}
	})

	t.Run("malformed ordinal is ignored", func(t *testing.T) {
		req := &csi.TopologyRequirement{
			Preferred: []*csi.Topology{topologyWithSegments(map[string]string{
				topologyKeyStorageNodeUUIDPrefix + uuid: clusterID + ".not-a-number",
			})},
		}
		if got := coLocatedHostID(req, clusterID); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}
