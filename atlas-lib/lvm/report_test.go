package lvm

import (
	"context"
	"strings"
	"testing"
)

func TestManager_pvsVGName_GarbageIsAParseErrorNotACorruptValue(t *testing.T) {
	key := joinKey([]string{"pvs", "--devices", "/dev/nvme0n1", "--reportformat", "json", "-o", "vg_name", "/dev/nvme0n1"})
	fake := &fakeRunner{out: map[string]string{key: "not json at all"}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	_, err := mgr.pvsVGName(context.Background(), "/dev/nvme0n1")
	if err == nil || !strings.Contains(err.Error(), "parse pvs report") {
		t.Fatalf("pvsVGName() error = %v, want a parse error", err)
	}
}

func TestManager_lvsLVNames_GarbageIsAParseErrorNotACorruptValue(t *testing.T) {
	key := joinKey([]string{"lvs", "--reportformat", "json", "-o", "lv_name", "vg1"})
	fake := &fakeRunner{out: map[string]string{key: "not json at all"}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	_, err := mgr.lvsLVNames(context.Background(), nil, "vg1")
	if err == nil || !strings.Contains(err.Error(), "parse lvs report") {
		t.Fatalf("lvsLVNames() error = %v, want a parse error", err)
	}
}
