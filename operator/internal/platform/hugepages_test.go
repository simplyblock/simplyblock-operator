package platform

import (
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// U-15..U-18 (test-plan-preprovisioned-hugepages.md).
func TestHugePagesModeSupported(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode simplyblockv1alpha2.HugePagesMode
		want bool
	}{
		{"unset", "", true},
		{"preprovisioned", simplyblockv1alpha2.HugePagesModePreprovisioned, true},
		{"exclusive", simplyblockv1alpha2.HugePagesModeExclusive, false},
		{"shared", simplyblockv1alpha2.HugePagesModeShared, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HugePagesModeSupported(tc.mode); got != tc.want {
				t.Errorf("HugePagesModeSupported(%q) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}
