// The phase the component table derives, and the asymmetry that keeps a fleet
// from being halted over an exporter.
//
// The pairs matter more than the individual rows. Only a component the table
// marks essential may produce Unavailable, because Unavailable holds every
// controller in the operator. Each case here is therefore stated twice, once for
// an essential component and once for a non-essential one at the same counts.

package controlplane

import (
	"context"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// A failing probe settles the phase on its own. A control plane that does not
// answer is Unavailable whatever its pod counts say, and the message is the
// control plane's own rather than a paraphrase of it.
func TestAFailingProbeIsUnavailableWhateverTheComponentsSay(t *testing.T) {
	everythingReady := []simplyblockv1alpha2.ControlPlaneComponentStatus{
		componentStatus(ComponentWebAPI, 2, 2, true),
		componentStatus(ComponentFDBCluster, 7, 7, true),
	}

	phase, message := derivePhase(false, "status=503: fdb unavailable", everythingReady)

	if phase != simplyblockv1alpha2.ControlPlanePhaseUnavailable {
		t.Errorf("phase = %s, want Unavailable", phase)
	}
	if message != "status=503: fdb unavailable" {
		t.Errorf("message = %q, want the control plane's own words", message)
	}
}

// An essential component at zero ready is Unavailable; a non-essential one at
// the same counts is Degraded. This is the asymmetry §4.3 calls the safety
// property, and it is the one row of this table worth getting wrong twice.
func TestOnlyAnEssentialComponentAtZeroReachesUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		essential bool
		want      simplyblockv1alpha2.ControlPlanePhase
	}{
		{"an essential component at zero", true, simplyblockv1alpha2.ControlPlanePhaseUnavailable},
		{"a non-essential component at zero", false, simplyblockv1alpha2.ControlPlanePhaseDegraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			components := []simplyblockv1alpha2.ControlPlaneComponentStatus{
				componentStatus("a-component", 1, 0, tc.essential),
			}

			phase, message := derivePhase(true, "", components)

			if phase != tc.want {
				t.Errorf("phase = %s, want %s", phase, tc.want)
			}
			if message == "" {
				t.Error("message is empty; a phase that cannot be explained is one nobody trusts")
			}
		})
	}
}

// A component below its desired count while above zero is Degraded whether or
// not it is essential. The management API at one of two replicas is still
// answering every request, which is exactly the window Degraded exists to name.
func TestBelowDesiredButAboveZeroIsDegradedEvenWhenEssential(t *testing.T) {
	for _, essential := range []bool{true, false} {
		components := []simplyblockv1alpha2.ControlPlaneComponentStatus{
			componentStatus(ComponentWebAPI, 2, 1, essential),
		}

		phase, _ := derivePhase(true, "", components)

		if phase != simplyblockv1alpha2.ControlPlanePhaseDegraded {
			t.Errorf("essential=%v: phase = %s, want Degraded", essential, phase)
		}
	}
}

// The phase is the worst verdict across every component, not the first or the
// last one read.
func TestThePhaseIsTheWorstVerdictAcrossEveryComponent(t *testing.T) {
	components := []simplyblockv1alpha2.ControlPlaneComponentStatus{
		componentStatus("healthy", 1, 1, false),
		componentStatus("degraded", 2, 1, false),
		componentStatus("down", 1, 0, true),
		componentStatus("also-healthy", 1, 1, true),
	}

	phase, message := derivePhase(true, "", components)

	if phase != simplyblockv1alpha2.ControlPlanePhaseUnavailable {
		t.Errorf("phase = %s, want Unavailable", phase)
	}
	if message != "down has 0 of 1 replicas ready" {
		t.Errorf("message = %q, want the component that produced the verdict", message)
	}
}

// A workload the apply just created and the cache has not caught up with reads
// as zero desired, which is nothing asked for rather than something missing. An
// install therefore settles without passing through Unavailable.
func TestZeroDesiredIsNotAnOutage(t *testing.T) {
	components := []simplyblockv1alpha2.ControlPlaneComponentStatus{
		componentStatus(ComponentWebAPI, 0, 0, true),
	}

	phase, _ := derivePhase(true, "", components)

	if phase != simplyblockv1alpha2.ControlPlanePhaseAvailable {
		t.Errorf("phase = %s, want Available: a workload that is not there yet is not an outage", phase)
	}
}

// A remote control plane has no components the operator owns, so the probe is
// the only signal and Degraded is unreachable.
func TestWithNoComponentsThePhaseFollowsTheProbeAlone(t *testing.T) {
	for _, tc := range []struct {
		probeOK bool
		want    simplyblockv1alpha2.ControlPlanePhase
	}{
		{true, simplyblockv1alpha2.ControlPlanePhaseAvailable},
		{false, simplyblockv1alpha2.ControlPlanePhaseUnavailable},
	} {
		phase, _ := derivePhase(tc.probeOK, "unreachable", nil)
		if phase != tc.want {
			t.Errorf("probeOK=%v: phase = %s, want %s", tc.probeOK, phase, tc.want)
		}
	}
}

// The management API and the database are the only components that may halt a
// fleet. This pins the closed list §4.3 says somebody has to have reviewed: a
// component added to the install without a decision about it lands in the
// non-essential default, and this test is what notices when one is added as
// essential instead.
func TestOnlyTheAPIAndTheDatabaseAreEssential(t *testing.T) {
	want := map[string]bool{
		ComponentWebAPI:     true,
		ComponentFDBCluster: true,
	}

	got := essentialComponents()

	if len(got) != len(want) {
		t.Fatalf("essential components = %v, want exactly %v", got, want)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%s is not essential, and its absence is an outage", name)
		}
	}
}

// Every component in the table carries the reason for its classification, so
// that the table explains itself where it is edited.
func TestEveryComponentStatesWhyItIsClassifiedAsItIs(t *testing.T) {
	for _, comp := range componentTable {
		if comp.why == "" {
			t.Errorf("%s states no reason for essential=%v", comp.name, comp.essential)
		}
	}
}

// The FoundationDBCluster is not something a Restart rolls: recycling a database
// is the FoundationDB operator's mechanism rather than a pod-template
// annotation, and a restart that silently skipped it would report success having
// done nothing to it.
func TestTheDatabaseIsNotRestartable(t *testing.T) {
	for _, comp := range restartableComponents() {
		if comp.name == ComponentFDBCluster {
			t.Fatalf("%s is restartable, and writing a pod-template annotation onto a "+
				"FoundationDBCluster does nothing", ComponentFDBCluster)
		}
	}
	if restartable(ComponentFDBCluster) {
		t.Errorf("%s is accepted as a restart scope, so an operation naming it would recycle "+
			"nothing and report success", ComponentFDBCluster)
	}
	if !restartable(ComponentTasks) {
		t.Errorf("%s is refused as a restart scope, and it is a workload a restart rolls",
			ComponentTasks)
	}
}

// Every component the table lists is either restartable or excluded for a stated
// reason. A component that is neither is one a Restart refuses without anything
// saying why.
func TestEveryComponentIsRestartableOrExcludedForAReason(t *testing.T) {
	for _, comp := range componentTable {
		if comp.kind == kindFoundationDB {
			continue
		}
		if !restartable(comp.name) {
			t.Errorf("%s is neither a FoundationDB resource nor restartable", comp.name)
		}
	}
}

// A workload that is not in the cluster reads as zero against zero rather than
// as an error, because the apply that creates it and the read that follows are
// two calls with a cache between them.
func TestObserveReadsAnAbsentWorkloadAsNothingAskedFor(t *testing.T) {
	c := newClient(t)

	components, err := observe(context.Background(), c, testNamespace)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	if len(components) != len(componentTable) {
		t.Fatalf("observe reported %d components, want one per table entry (%d)",
			len(components), len(componentTable))
	}
	for _, status := range components {
		if status.Desired != 0 || status.Ready != 0 {
			t.Errorf("%s = %d/%d ready, want 0/0 for a workload that does not exist",
				status.Name, status.Ready, status.Desired)
		}
	}
}

// observe reads the counts off the workloads, and carries each component's
// classification through to the status so a phase can be explained without
// reading the operator's source.
func TestObserveReportsTheCountsAndTheClassification(t *testing.T) {
	c := newClient(t,
		deployment(ComponentWebAPI, 2, 1),
		statefulSet(ComponentMinio, 1, 1),
	)

	components, err := observe(context.Background(), c, testNamespace)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	byName := map[string]simplyblockv1alpha2.ControlPlaneComponentStatus{}
	for _, status := range components {
		byName[status.Name] = status
	}

	api := byName[ComponentWebAPI]
	if api.Desired != 2 || api.Ready != 1 {
		t.Errorf("%s = %d/%d ready, want 1/2", ComponentWebAPI, api.Ready, api.Desired)
	}
	if !api.Essential {
		t.Errorf("%s is reported non-essential", ComponentWebAPI)
	}

	store := byName[ComponentMinio]
	if store.Desired != 1 || store.Ready != 1 {
		t.Errorf("%s = %d/%d ready, want 1/1", ComponentMinio, store.Ready, store.Desired)
	}
	if store.Essential {
		t.Errorf("%s is reported essential, which would let the object store halt a fleet",
			ComponentMinio)
	}
}
