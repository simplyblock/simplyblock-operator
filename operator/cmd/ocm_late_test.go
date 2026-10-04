package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// scriptedDiscovery answers ServerResourcesForGroupVersion from a script, one
// answer per call; the last answer repeats.
type scriptedDiscovery struct {
	answers []func() (*metav1.APIResourceList, error)
	calls   int
}

func (d *scriptedDiscovery) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	i := d.calls
	if i >= len(d.answers) {
		i = len(d.answers) - 1
	}
	d.calls++
	return d.answers[i]()
}

func notServed() (*metav1.APIResourceList, error) {
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "work.open-cluster-management.io"}, "v1")
}

func servedWithout() (*metav1.APIResourceList, error) {
	// a managed cluster: the group serves AppliedManifestWork, not ManifestWork
	return &metav1.APIResourceList{APIResources: []metav1.APIResource{{Name: "appliedmanifestworks"}}}, nil
}

func served() (*metav1.APIResourceList, error) {
	return &metav1.APIResourceList{APIResources: []metav1.APIResource{{Name: ocmManifestWorkResource}}}, nil
}

func discoveryError() (*metav1.APIResourceList, error) {
	return nil, errors.New("the server is currently unable to handle the request")
}

// instant fires every wait at once and records the intervals asked for.
func instant(waits *[]time.Duration) func(time.Duration) <-chan time.Time {
	return func(d time.Duration) <-chan time.Time {
		*waits = append(*waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
}

func TestWaitForResourceFindsManifestWorkInstalledLater(t *testing.T) {
	disc := &scriptedDiscovery{answers: []func() (*metav1.APIResourceList, error){
		notServed, servedWithout, discoveryError, served}}
	var waits []time.Duration
	ok := waitForResource(context.Background(), logr.Discard(), disc, ocmWorkGroupVersion, ocmManifestWorkResource,
		time.Minute, 3*time.Minute, instant(&waits))
	if !ok {
		t.Fatal("waitForResource gave up although ManifestWork became served")
	}
	if disc.calls != 4 {
		t.Errorf("discovery called %d times, want 4 (absent, other kind only, error, served)", disc.calls)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute, 3 * time.Minute}
	if len(waits) != len(want) {
		t.Fatalf("waits %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait %d = %v, want %v (doubling, capped)", i, waits[i], want[i])
		}
	}
}

func TestWaitForResourceStopsWithItsContext(t *testing.T) {
	disc := &scriptedDiscovery{answers: []func() (*metav1.APIResourceList, error){notServed}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	never := func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	if waitForResource(ctx, logr.Discard(), disc, ocmWorkGroupVersion, ocmManifestWorkResource,
		time.Minute, time.Minute, never) {
		t.Fatal("waitForResource reported the resource served after its context ended")
	}
	if disc.calls != 0 {
		t.Errorf("discovery called %d times after cancellation, want 0", disc.calls)
	}
}

func TestOCMLateStartRegistersTheControllersOnce(t *testing.T) {
	disc := &scriptedDiscovery{answers: []func() (*metav1.APIResourceList, error){notServed, served}}
	var waits []time.Duration
	registered := 0
	o := &ocmLateStart{log: logr.Discard(), disc: disc, after: instant(&waits),
		register: func() error { registered++; return nil }}
	if err := o.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if registered != 1 {
		t.Errorf("controllers registered %d times, want 1", registered)
	}
}

func TestOCMLateStartReportsARegistrationFailure(t *testing.T) {
	disc := &scriptedDiscovery{answers: []func() (*metav1.APIResourceList, error){served}}
	var waits []time.Duration
	o := &ocmLateStart{log: logr.Discard(), disc: disc, after: instant(&waits),
		register: func() error { return errors.New("boom") }}
	if err := o.Start(context.Background()); err == nil {
		t.Fatal("Start hid the registration failure")
	}
}
