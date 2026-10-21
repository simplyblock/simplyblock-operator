// One pass over a storage cluster's metadata server: the state volume, the
// allowed hosts, the scale, and the warning for a missing standby.

package mdsstate

import (
	"context"
	"errors"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/lvol"
)

type recordedEvent struct{ eventType, reason, message string }

type eventLog struct{ events []recordedEvent }

func (l *eventLog) record(eventType, reason, message string) {
	l.events = append(l.events, recordedEvent{eventType, reason, message})
}

func (l *eventLog) has(reason string) bool {
	return slices.ContainsFunc(l.events, func(e recordedEvent) bool { return e.reason == reason })
}

func coordinatorFor(t *testing.T, api *fakeAPI, sts *appsv1.StatefulSet, sw Switchover) (*Coordinator, *eventLog) {
	t.Helper()
	log := &eventLog{}
	c := kubeFor(t, sts)
	return &Coordinator{
		Client:     c,
		APIFor:     func(context.Context, string) (VolumeAPI, error) { return api, nil },
		Switchover: sw,
		Events:     log.record,
	}, log
}

func readyPods(names ...string) []corev1.Pod {
	var out []corev1.Pod
	for _, n := range names {
		out = append(out, pod(n, true, false))
	}
	return out
}

// The first export's pool holds the state volume, its host list gets every
// ordinal, and the StatefulSet is scaled.
func TestReconcileCreatesTheStateVolumeAllowsHostsAndScales(t *testing.T) {
	api := newFakeAPI()
	first := api.add("export-1", "nqn.client")
	second := api.add("export-2")
	sts := stsWithReplicas(1)
	co, log := coordinatorFor(t, api, sts, &fakeSwitchover{known: false})

	res, err := co.Reconcile(context.Background(), Input{
		StatefulSet: sts, ClusterID: testCluster, Standbys: 1,
		Exports: []lvol.VolumeHandle{first, second}, Pods: readyPods("mds-0", "mds-1"),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := reload(t, co.Client, sts)
	if res.StateVolume == "" || got.Annotations[StateVolumeAnnotation] != string(res.StateVolume) {
		t.Errorf("state volume %q, recorded %q", res.StateVolume, got.Annotations[StateVolumeAnnotation])
	}
	if !slices.Contains(api.hostsOf(first), hostOf(t, 1)) {
		t.Errorf("export 1 hosts = %v, want ordinal 1 allowed", api.hostsOf(first))
	}
	if len(api.hostsOf(second)) != 0 {
		t.Errorf("an open export got hosts: %v", api.hostsOf(second))
	}
	if *got.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *got.Spec.Replicas)
	}
	if log.has("MDSStateUnavailable") || log.has("MDSStandbyMissing") {
		t.Errorf("unexpected events: %+v", log.events)
	}
}

// O-05: a failing create is an event and a retry, nothing recorded, and the
// volumes that do not need the state volume are still handled.
func TestReconcileStateVolumeFailureIsAnEventAndARetry(t *testing.T) {
	api := newFakeAPI()
	export := api.add("export", "nqn.client")
	api.createErr = errors.New("500")
	sts := stsWithReplicas(2)
	co, log := coordinatorFor(t, api, sts, &fakeSwitchover{})

	res, err := co.Reconcile(context.Background(), Input{
		StatefulSet: sts, ClusterID: testCluster, Standbys: 1,
		Exports: []lvol.VolumeHandle{export}, Pods: readyPods("mds-0", "mds-1"),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !log.has("MDSStateUnavailable") || res.RequeueAfter == 0 || res.StateVolume != "" {
		t.Errorf("events %+v, requeue %v, state %q: want MDSStateUnavailable and a retry", log.events, res.RequeueAfter, res.StateVolume)
	}
	if !slices.Contains(api.hostsOf(export), hostOf(t, 1)) {
		t.Error("the export's hosts waited on the state volume")
	}
}

// A volume whose hosts cannot be updated is an event and a retry, and does not
// keep the other volumes from being usable.
func TestReconcileHostFailureIsAnEventAndARetry(t *testing.T) {
	api := newFakeAPI()
	bad := api.add("bad", "nqn.client")
	good := api.add("good", "nqn.client")
	api.hostErr[bad] = errors.New("503")
	sts := stsWithReplicas(2)
	co, log := coordinatorFor(t, api, sts, &fakeSwitchover{})

	res, err := co.Reconcile(context.Background(), Input{
		StatefulSet: sts, ClusterID: testCluster, Standbys: 1,
		Exports: []lvol.VolumeHandle{bad, good}, Pods: readyPods("mds-0", "mds-1"),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !log.has("MDSHostNotAllowed") || res.RequeueAfter == 0 {
		t.Errorf("events %+v, requeue %v: want MDSHostNotAllowed and a retry", log.events, res.RequeueAfter)
	}
	if res.Hosts.Allowed(bad) || !res.Hosts.Allowed(good) {
		t.Errorf("Allowed(bad) = %v, Allowed(good) = %v", res.Hosts.Allowed(bad), res.Hosts.Allowed(good))
	}
}

func TestReconcileWarnsWhenNoStandbyIsReady(t *testing.T) {
	api := newFakeAPI()
	sts := stsWithReplicas(2)
	sts.Annotations = map[string]string{ActiveAnnotation: "mds-0"}
	co, log := coordinatorFor(t, api, sts, &fakeSwitchover{known: true})

	if _, err := co.Reconcile(context.Background(), Input{
		StatefulSet: sts, ClusterID: testCluster, Standbys: 1, Pods: append(readyPods("mds-0"), pod("mds-1", false, false)),
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var found *recordedEvent
	for i := range log.events {
		if log.events[i].reason == "MDSStandbyMissing" {
			found = &log.events[i]
		}
	}
	if found == nil || found.eventType != corev1.EventTypeWarning {
		t.Errorf("events %+v, want a MDSStandbyMissing warning", log.events)
	}
}

func TestReconcileDoesNotWarnWithoutStandbys(t *testing.T) {
	sts := stsWithReplicas(1)
	co, log := coordinatorFor(t, newFakeAPI(), sts, &fakeSwitchover{known: true})
	if _, err := co.Reconcile(context.Background(), Input{
		StatefulSet: sts, ClusterID: testCluster, Standbys: 0, Pods: readyPods("mds-0"),
	}); err != nil {
		t.Fatal(err)
	}
	if log.has("MDSStandbyMissing") {
		t.Errorf("events %+v", log.events)
	}
}

// A scale-down held for the switchover is a retry, not a failure.
func TestReconcileScaleDownWaitsForTheSwitchover(t *testing.T) {
	sts := stsWithReplicas(2)
	sw := &fakeSwitchover{known: true, active: 1}
	co, _ := coordinatorFor(t, newFakeAPI(), sts, sw)

	res, err := co.Reconcile(context.Background(), Input{StatefulSet: sts, ClusterID: testCluster, Standbys: 0, Pods: readyPods("mds-0", "mds-1")})
	if err != nil || res.Scale != ScaleWaitingForSwitchover || res.RequeueAfter == 0 {
		t.Errorf("Reconcile = %+v, %v, want a held scale-down and a retry", res, err)
	}
	if len(sw.requested) != 1 {
		t.Errorf("switchover requests = %v", sw.requested)
	}
}

// Without a hook the scale-down is held with an event, not applied.
func TestReconcileScaleDownWithoutAHookIsHeldWithAnEvent(t *testing.T) {
	sts := stsWithReplicas(2)
	co, log := coordinatorFor(t, newFakeAPI(), sts, nil)
	res, err := co.Reconcile(context.Background(), Input{StatefulSet: sts, ClusterID: testCluster, Standbys: 0, Pods: readyPods("mds-0", "mds-1")})
	if err != nil || res.RequeueAfter == 0 || !log.has("MDSScaleHeld") {
		t.Errorf("Reconcile = %+v, %v, events %+v, want a held scale-down", res, err, log.events)
	}
	if *reload(t, co.Client, sts).Spec.Replicas != 2 {
		t.Error("the StatefulSet was scaled down without knowing the active")
	}
}

// With nothing to authorize against, the control plane is not needed and the
// scale still happens.
func TestReconcileWithNoExportsAndNoStateVolumeTouchesNoVolume(t *testing.T) {
	api := newFakeAPI()
	sts := stsWithReplicas(1)
	co, _ := coordinatorFor(t, api, sts, &fakeSwitchover{})
	res, err := co.Reconcile(context.Background(), Input{StatefulSet: sts, ClusterID: testCluster, Standbys: 1, Pods: readyPods("mds-0", "mds-1")})
	if err != nil || res.StateVolume != "" || api.creates != 0 {
		t.Errorf("Reconcile = %+v, %v, %d creates, want no volume without an export", res, err, api.creates)
	}
	if *reload(t, co.Client, sts).Spec.Replicas != 2 {
		t.Error("not scaled")
	}
}

func TestFinalizeDeletesTheStateVolumeThroughTheCoordinator(t *testing.T) {
	api := newFakeAPI()
	h := api.add("state")
	sts := testSTS(map[string]string{StateVolumeAnnotation: string(h)})
	sts.Finalizers = []string{StateVolumeFinalizer}
	co, _ := coordinatorFor(t, api, sts, nil)
	if err := co.Finalize(context.Background(), sts, testCluster); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if _, ok := api.volumes[h]; ok {
		t.Error("the volume still exists")
	}
}
