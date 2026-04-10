package controller

import (
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
)

func TestLvolStatusListFromAPIAndNormalize(t *testing.T) {
	api := []LVOLAPIResponse{
		{
			UUID:          "b",
			LvolName:      "lvol-b",
			NodeUUID:      []string{"node-2", "node-1"},
			Size:          1024,
			Status:        "online",
			PoolName:      "pool-a",
			PoolUUID:      "pool-uuid",
			QosIOPS:       10,
			QosRWTP:       20,
			QosRTP:        30,
			QosWTP:        40,
			QosClass:      1,
			StripeWdata:   1,
			StripeWparity: 1,
		},
		{
			UUID:          "a",
			LvolName:      "lvol-a",
			NodeUUID:      []string{"node-3", "node-1"},
			Size:          2048,
			Status:        "online",
			PoolName:      "pool-a",
			PoolUUID:      "pool-uuid",
			QosIOPS:       11,
			QosRWTP:       21,
			QosRTP:        31,
			QosWTP:        41,
			QosClass:      2,
			StripeWdata:   1,
			StripeWparity: 1,
		},
	}

	status := lvolStatusListFromAPI(api)
	if len(status.Lvols) != 2 {
		t.Fatalf("expected 2 lvols, got %d", len(status.Lvols))
	}

	normalizeLvolStatus(&status)
	if status.Lvols[0].UUID != "a" || status.Lvols[1].UUID != "b" {
		t.Fatalf("lvols should be sorted by UUID: %#v", status.Lvols)
	}

	for _, l := range status.Lvols {
		if len(l.NodeUUID) < 2 {
			t.Fatalf("unexpected nodeUUID list: %#v", l.NodeUUID)
		}
		if l.NodeUUID[0] > l.NodeUUID[1] {
			t.Fatalf("nodeUUID should be sorted: %#v", l.NodeUUID)
		}
	}
}

func TestNormalizeLvolStatusEmptySafe(t *testing.T) {
	s := simplyblockv1alpha1.LvolStatus{}
	normalizeLvolStatus(&s)
	if len(s.Lvols) != 0 {
		t.Fatalf("expected empty lvols")
	}
}
