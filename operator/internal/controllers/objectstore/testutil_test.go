// Small polling and value helpers every test in this package reaches for: none
// of this package's fakes are watches, so a test proving one of them has to
// poll for the effect rather than block on a channel, the same shape
// atlas-lib/link's own tests use for the same reason (an in-process fake
// standing in for something that would otherwise need a live cluster).

package objectstore

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// waitFor polls cond until it holds, failing the test if it never does.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// neverWithin fails if cond becomes true inside d. It is how a test asserts a
// negative: nothing happening is the expected outcome, and only time
// distinguishes that from something that has not happened yet.
func neverWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("expected %s never to happen, but it did", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mustQuantity parses a resource.Quantity literal, panicking on a malformed
// one: these are fixed test fixtures, never user input, so a parse failure is
// a typo in the test itself.
func mustQuantity(s string) resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic("mustQuantity: " + s + ": " + err.Error())
	}
	return q
}
