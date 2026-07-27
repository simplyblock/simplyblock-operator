package autoplacement

import (
	"math"
	"sort"
)

const (
	// madToSigma scales the median absolute deviation (MAD) to an estimate of the standard
	// deviation for normally distributed data; it makes the Hampel k threshold comparable to
	// a number of standard deviations.
	madToSigma = 1.4826
	// meanAbsDevToSigma is the equivalent scale factor for the mean absolute deviation, used
	// only in the degenerate MAD==0 fallback.
	meanAbsDevToSigma = 1.2533
)

// robustBaselineNS reduces a set of latency samples (ns) to a single baseline using the
// Hampel identifier: samples further than k·1.4826·MAD from the window median are rejected
// as outliers, and the median of the survivors is returned. The Hampel identifier has the
// highest possible breakdown point (50%) — both its centre (median) and its scale (MAD) are
// themselves robust, so the extreme journal/EC/HA spikes it is meant to reject cannot inflate
// the threshold and hide themselves.
//
// Degrade rules:
//   - fewer than 3 samples: rejection is not meaningful, so the plain median is returned;
//   - MAD == 0 (a majority of identical samples): fall back to the mean absolute deviation,
//     and if that is also 0 (all samples identical) skip rejection entirely.
//
// kept and rejected count survivors and rejects (kept+rejected == len(samples)). ok is false
// only when samples is empty.
func robustBaselineNS(samples []float64, k float64) (baselineNS int64, kept, rejected int, ok bool) {
	if len(samples) == 0 {
		return 0, 0, 0, false
	}
	if len(samples) < 3 {
		return int64(math.Round(median(samples))), len(samples), 0, true
	}

	m := median(samples)
	scale := madToSigma * medianAbsDev(samples, m)
	if scale == 0 {
		// MAD degenerate (majority identical): fall back to the mean absolute deviation.
		scale = meanAbsDevToSigma * meanAbsDev(samples, m)
	}
	if scale == 0 {
		// All samples identical: nothing to reject.
		return int64(math.Round(m)), len(samples), 0, true
	}

	threshold := k * scale
	survivors := make([]float64, 0, len(samples))
	for _, x := range samples {
		if math.Abs(x-m) <= threshold {
			survivors = append(survivors, x)
		}
	}
	// The median always survives, so survivors is never empty; guard defensively anyway.
	if len(survivors) == 0 {
		survivors = samples
	}
	return int64(math.Round(median(survivors))), len(survivors), len(samples) - len(survivors), true
}

// median returns the median of xs without mutating it. Returns 0 for an empty slice.
func median(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, xs)
	sort.Float64s(sorted)
	mid := n / 2
	if n%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// medianAbsDev returns the median of the absolute deviations of xs from center.
func medianAbsDev(xs []float64, center float64) float64 {
	devs := make([]float64, len(xs))
	for i, x := range xs {
		devs[i] = math.Abs(x - center)
	}
	return median(devs)
}

// meanAbsDev returns the mean of the absolute deviations of xs from center.
func meanAbsDev(xs []float64, center float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += math.Abs(x - center)
	}
	return sum / float64(len(xs))
}
