// Shared assertions for the conversion tests in this package.
//
// Every kind that recases an enum needs the same check — each value converts up
// to its hub spelling and back down to the one it shipped with — and written out
// per kind it is the same thirty lines each time. What differs between kinds is
// only which field is read, so that is what the callers supply.

package v1alpha1

import "testing"

// enumPair is one value under both spellings: as v1alpha1 wrote it, and as the
// hub does.
type enumPair struct {
	v1alpha1 string
	hub      string
}

// assertEnumConvertsBothWays checks each pair in both directions.
//
// Both directions matter, and a one-way test cannot see the bug that motivates
// this: a conversion that maps going up and copies going down corrupts the value
// on the first `kubectl get -o yaml | kubectl apply -f -`, which is a round trip
// through exactly these two functions.
func assertEnumConvertsBothWays(
	t *testing.T,
	pairs []enumPair,
	up func(t *testing.T, v1alpha1Value string) string,
	down func(t *testing.T, hubValue string) string,
) {
	t.Helper()

	for _, pair := range pairs {
		t.Run(pair.v1alpha1, func(t *testing.T) {
			if got := up(t, pair.v1alpha1); got != pair.hub {
				t.Errorf("up: %q converted to %q, want %q", pair.v1alpha1, got, pair.hub)
			}
			if got := down(t, pair.hub); got != pair.v1alpha1 {
				t.Errorf("down: %q converted to %q, want %q", pair.hub, got, pair.v1alpha1)
			}
		})
	}
}
