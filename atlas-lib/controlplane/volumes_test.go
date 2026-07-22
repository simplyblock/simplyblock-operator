package controlplane

import (
	"math"
	"testing"
)

func TestSizeToInt(t *testing.T) {
	if got, err := sizeToInt(1 << 30); err != nil || got != 1<<30 {
		t.Errorf("sizeToInt(1GiB) = %d, %v; want 1073741824, nil", got, err)
	}
	if _, err := sizeToInt(math.MaxUint64); err == nil {
		t.Error("sizeToInt(MaxUint64) = nil error, want overflow error")
	}
}
