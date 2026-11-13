package main

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Ramen is installed after the operator on a stretched site (regression_test/25
// 08): the VRG follower starts once the VolumeReplicationGroup kind is served,
// without restarting the operator, and never while it is absent, because a watch
// on a kind the API server does not serve stops the whole manager.
func TestTheVRGFollowerStartsOnceRamenIsInstalled(t *testing.T) {
	vrgServed := func() (*metav1.APIResourceList, error) {
		return &metav1.APIResourceList{APIResources: []metav1.APIResource{{Name: ramenVRGResource}}}, nil
	}
	disc := &scriptedDiscovery{answers: []func() (*metav1.APIResourceList, error){notServed, notServed, vrgServed}}
	registered := 0
	var waits []time.Duration
	late := &ramenLateStart{log: logr.Discard(), disc: disc, register: func() error { registered++; return nil },
		after: instant(&waits)}
	if err := late.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if registered != 1 || disc.calls != 3 {
		t.Errorf("registered %d times after %d discovery calls; want once, after the kind appeared", registered, disc.calls)
	}
}

// The follower is never registered when the operator stops before Ramen comes.
func TestTheVRGFollowerIsNotStartedWithoutRamen(t *testing.T) {
	disc := &scriptedDiscovery{answers: []func() (*metav1.APIResourceList, error){notServed}}
	ctx, cancel := context.WithCancel(context.Background())
	registered := 0
	late := &ramenLateStart{log: logr.Discard(), disc: disc, register: func() error { registered++; return nil },
		after: func(time.Duration) <-chan time.Time { cancel(); return make(chan time.Time) }}
	if err := late.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if registered != 0 {
		t.Error("the follower was registered with no VolumeReplicationGroup kind served")
	}
}
