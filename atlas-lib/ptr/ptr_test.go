package ptr

import "testing"

func TestTo(t *testing.T) {
	if got := To(5); got == nil || *got != 5 {
		t.Errorf("To(5) = %v, want *5", got)
	}
}

func TestFrom(t *testing.T) {
	if got := From((*int)(nil), 7); got != 7 {
		t.Errorf("From(nil, 7) = %d, want 7", got)
	}
	v := 3
	if got := From(&v, 7); got != 3 {
		t.Errorf("From(&3, 7) = %d, want 3", got)
	}
}
