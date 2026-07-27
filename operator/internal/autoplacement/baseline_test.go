package autoplacement

import "testing"

func TestRobustBaselineNS_Empty(t *testing.T) {
	_, _, _, ok := robustBaselineNS(nil, 3.0)
	if ok {
		t.Fatalf("expected ok=false for empty samples")
	}
}

func TestRobustBaselineNS_FewSamplesPlainMedian(t *testing.T) {
	// Under 3 samples: no rejection possible, returns the plain median.
	got, kept, rejected, ok := robustBaselineNS([]float64{100, 300}, 3.0)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got != 200 {
		t.Errorf("baseline = %d, want 200 (median of 100,300)", got)
	}
	if kept != 2 || rejected != 0 {
		t.Errorf("kept=%d rejected=%d, want 2/0", kept, rejected)
	}
}

func TestRobustBaselineNS_RejectsHighSpikes(t *testing.T) {
	// A calm cluster around ~1000ns with two large journal/EC-style spikes. The Hampel
	// identifier should reject the spikes and land the baseline on the calm median.
	samples := []float64{980, 1000, 1010, 990, 1005, 995, 1000, 50000, 48000}
	got, kept, rejected, ok := robustBaselineNS(samples, 3.0)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if rejected != 2 {
		t.Errorf("rejected = %d, want 2 (the two spikes)", rejected)
	}
	if kept != len(samples)-2 {
		t.Errorf("kept = %d, want %d", kept, len(samples)-2)
	}
	if got < 950 || got > 1050 {
		t.Errorf("baseline = %d, want ~1000 (calm median, spikes rejected)", got)
	}
}

func TestRobustBaselineNS_AllIdenticalMADZeroGuard(t *testing.T) {
	// MAD == 0 (all identical) must not reject everything — it returns the common value.
	got, kept, rejected, ok := robustBaselineNS([]float64{1000, 1000, 1000, 1000, 1000}, 3.0)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got != 1000 {
		t.Errorf("baseline = %d, want 1000", got)
	}
	if rejected != 0 || kept != 5 {
		t.Errorf("kept=%d rejected=%d, want 5/0", kept, rejected)
	}
}

func TestRobustBaselineNS_MajorityIdenticalMeanAbsDevFallback(t *testing.T) {
	// A majority are identical so the MAD is 0; the mean-absolute-deviation fallback still
	// yields a positive scale and rejects the lone far outlier.
	samples := []float64{1000, 1000, 1000, 1000, 1000, 1000, 90000}
	got, _, rejected, ok := robustBaselineNS(samples, 3.0)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if rejected != 1 {
		t.Errorf("rejected = %d, want 1 (the outlier)", rejected)
	}
	if got != 1000 {
		t.Errorf("baseline = %d, want 1000", got)
	}
}

func TestRobustBaselineNS_CleanDataKeepsAll(t *testing.T) {
	// Tight, well-behaved data: nothing should be rejected.
	samples := []float64{1000, 1010, 990, 1005, 995, 1002, 998}
	got, kept, rejected, ok := robustBaselineNS(samples, 3.0)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if rejected != 0 || kept != len(samples) {
		t.Errorf("kept=%d rejected=%d, want %d/0", kept, rejected, len(samples))
	}
	if got != 1000 {
		t.Errorf("baseline = %d, want 1000 (median)", got)
	}
}

func TestMedian(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		want float64
	}{
		{"empty", nil, 0},
		{"single", []float64{5}, 5},
		{"odd", []float64{3, 1, 2}, 2},
		{"even", []float64{4, 1, 3, 2}, 2.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := median(c.in); got != c.want {
				t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestMedianDoesNotMutateInput(t *testing.T) {
	in := []float64{3, 1, 2}
	_ = median(in)
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Errorf("median mutated its input: %v", in)
	}
}
