// The failure-domain layout the control plane activates, written out from
// sbcli's activation gate (cluster_ops.cluster_activate and
// planner.fd_activation_domain_count_violation) rather than derived.

package erasurecoding

import (
	"strings"
	"testing"
)

func TestAFailureDomainLayoutNeedsParityPlusTwoDomains(t *testing.T) {
	for _, tc := range []struct {
		scheme Scheme
		hosts  map[string]int
		want   string
	}{
		{Scheme{2, 1}, map[string]int{"default": 4}, "at least 3"},
		{Scheme{2, 1}, map[string]int{"a": 2, "b": 2}, "at least 3"},
		{Scheme{1, 0}, map[string]int{"a": 1}, "at least 2"},
		{Scheme{2, 2}, map[string]int{"a": 1, "b": 1, "c": 1}, "at least 4"},
		{Scheme{2, 1}, map[string]int{"a": 2, "b": 1, "c": 1}, "equal"},
		{Scheme{2, 1}, map[string]int{"a": 1, "b": 1, "c": 1}, ""},
		{Scheme{1, 0}, map[string]int{"a": 2, "b": 2}, ""},
		{Scheme{2, 2}, map[string]int{"a": 2, "b": 2, "c": 2, "d": 2}, ""},
	} {
		got := FailureDomainViolation(tc.scheme, tc.hosts)
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s over %v = %q, want %q", tc.scheme, tc.hosts, got, tc.want)
		}
	}
}
