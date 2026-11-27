// Tests for starting the StorageProfile controller once CDI is served.

package main

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OpenShift Virtualization is commonly installed after simplyblock: the
// StorageProfile controller starts once CDI's kind is served, without
// restarting the operator.
func TestTheStorageProfileControllerStartsOnceCDIIsInstalled(t *testing.T) {
	served := func() (*metav1.APIResourceList, error) {
		return &metav1.APIResourceList{APIResources: []metav1.APIResource{{Name: cdiStorageProfileResource}}}, nil
	}
	disc := &scriptedDiscovery{answers: []func() (*metav1.APIResourceList, error){notServed, served}}
	registered := 0
	var waits []time.Duration
	late := &cdiLateStart{log: logr.Discard(), disc: disc, register: func() error { registered++; return nil },
		after: instant(&waits)}
	if err := late.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if registered != 1 || disc.calls != 2 {
		t.Errorf("registered %d times after %d discovery calls; want once, after the kind appeared", registered, disc.calls)
	}
}

// The controller is never registered on a cluster without CDI: a watch on a
// kind the API server does not serve stops the whole manager.
func TestTheStorageProfileControllerIsNotStartedWithoutCDI(t *testing.T) {
	disc := &scriptedDiscovery{answers: []func() (*metav1.APIResourceList, error){notServed}}
	ctx, cancel := context.WithCancel(context.Background())
	registered := 0
	late := &cdiLateStart{log: logr.Discard(), disc: disc, register: func() error { registered++; return nil },
		after: func(time.Duration) <-chan time.Time { cancel(); return make(chan time.Time) }}
	if err := late.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if registered != 0 {
		t.Errorf("registered %d times without CDI; want never", registered)
	}
}
