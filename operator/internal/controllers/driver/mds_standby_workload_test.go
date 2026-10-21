// The metadata server's StatefulSet as design-pnfs-mds-standby.md §5.1 and
// §5.4 describe it: one pod per guest, each on a node of its own, no state
// disk, and a template update that leaves the active record alone (O-01 to
// O-03, O-33, O-46).

package driver

import (
	"context"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/mdsstate"
)

// O-01, O-02, O-33, O-34: a guest per standby and one for the active.
func TestMDSStatefulSetRunsTheActiveAndEachStandby(t *testing.T) {
	for _, tc := range []struct {
		name     string
		standbys *int32
		want     int32
	}{
		{"unset takes the default of one standby", nil, 2},
		{"no standby", ptr.To(int32(0)), 1},
		{"one standby", ptr.To(int32(1)), 2},
		{"two standbys", ptr.To(int32(2)), 3},
		{"four standbys", ptr.To(int32(4)), 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := withMDS(testDriver("simplyblock"))
			d.Spec.PNFS.MDS.Standbys = tc.standbys
			_, sts := mdsObjects(t, d)
			if sts.Spec.Replicas == nil || *sts.Spec.Replicas != tc.want {
				t.Errorf("replicas = %v, want %d", sts.Spec.Replicas, tc.want)
			}
		})
	}
}

// Parallel, so a pod stuck on a lost node does not stop the other ordinal from
// starting, and OnDelete, because the operator rolls the pods itself: the
// StatefulSet controller restarts ordinals in a fixed order whichever is active.
func TestMDSStatefulSetStartsPodsTogetherAndLeavesRollsToTheOperator(t *testing.T) {
	_, sts := mdsObjects(t, withMDS(testDriver("simplyblock")))
	if sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement {
		t.Errorf("podManagementPolicy = %q, want Parallel", sts.Spec.PodManagementPolicy)
	}
	if sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
		t.Errorf("updateStrategy = %q, want OnDelete", sts.Spec.UpdateStrategy.Type)
	}
}

// Two guests on one node are lost together with it, so the pods require
// distinct nodes, in addition to the KVM selector.
func TestMDSPodsRequireNodesOfTheirOwn(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	_, sts := mdsObjects(t, d)
	pod := sts.Spec.Template.Spec

	anti := pod.Affinity
	if anti == nil || anti.PodAntiAffinity == nil {
		t.Fatalf("affinity = %+v, want a pod anti-affinity", anti)
	}
	terms := anti.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(terms) != 1 {
		t.Fatalf("required anti-affinity terms = %+v, want one", terms)
	}
	if terms[0].TopologyKey != corev1.LabelHostname {
		t.Errorf("topologyKey = %q, want %s", terms[0].TopologyKey, corev1.LabelHostname)
	}
	selector, err := metav1.LabelSelectorAsSelector(terms[0].LabelSelector)
	if err != nil {
		t.Fatalf("selector: %v", err)
	}
	if !selector.Matches(labelsSet(sts.Spec.Template.Labels)) {
		t.Errorf("the term's selector %v does not match the StatefulSet's own pods %v",
			terms[0].LabelSelector, sts.Spec.Template.Labels)
	}
	if selector.Matches(labelsSet(map[string]string{"app": "someone-else"})) {
		t.Errorf("the term's selector %v matches another workload's pods", terms[0].LabelSelector)
	}
	if pod.NodeSelector[KVMCapableLabel] != KVMCapableValue {
		t.Errorf("nodeSelector = %v, want the kvm-capable label too", pod.NodeSelector)
	}
}

// O-03: the client-recovery database is a simplyblock volume the guest connects
// itself, so the pod has no claim, no block device, and no state volume mount.
func TestMDSPodHasNoStateDisk(t *testing.T) {
	_, sts := mdsObjects(t, withMDS(testDriver("simplyblock")))
	if n := len(sts.Spec.VolumeClaimTemplates); n != 0 {
		t.Errorf("%d claim templates, want none", n)
	}
	c := runnerContainer(t, sts)
	if len(c.VolumeDevices) != 0 {
		t.Errorf("volumeDevices = %+v, want none", c.VolumeDevices)
	}
}

// Every guest boots with the StatefulSet's name as its hostname, so a client of
// one ordinal reclaims on another under the same server owner.
func TestMDSRunnerIsToldTheStatefulSetName(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	_, sts := mdsObjects(t, d)
	want := "--hostname=" + MDSStatefulSetName(d, testClusterID)
	args := runnerContainer(t, sts).Args
	if !slices.Contains(args, want) {
		t.Errorf("runner args = %v, want %q", args, want)
	}
	if !slices.Contains(args, "--pod-name=$(POD_NAME)") {
		t.Errorf("runner args = %v, want the pod's own name passed through the downward API", args)
	}
	env := runnerContainer(t, sts).Env
	for _, name := range []string{"POD_NAME", "POD_UID"} {
		if !slices.ContainsFunc(env, func(e corev1.EnvVar) bool { return e.Name == name && e.ValueFrom != nil }) {
			t.Errorf("env lacks %s from the downward API", name)
		}
	}
}

// The runner drains for 3s and gives the guest 20s to power down, so the grace
// period leaves both and a margin.
func TestMDSTerminationGraceCoversDrainAndShutdown(t *testing.T) {
	_, sts := mdsObjects(t, withMDS(testDriver("simplyblock")))
	grace := sts.Spec.Template.Spec.TerminationGracePeriodSeconds
	if grace == nil || *grace < 45 {
		t.Errorf("terminationGracePeriodSeconds = %v, want at least 45", grace)
	}
}

// The data network keeps the pods' anti-affinity: every pod carries the subnet
// requirement, and the pods still repel each other.
func TestMDSDataNetworkKeepsThePodAntiAffinity(t *testing.T) {
	d := withDataNetwork(testDriver("simplyblock"))
	_, sts, err := MDSObjects(d, testClusterID,
		&MDSDataAttachment{Network: "simplyblock-mds-data", Interface: testDataIface})
	if err != nil {
		t.Fatalf("MDSObjects: %v", err)
	}
	affinity := sts.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil || affinity.PodAntiAffinity == nil {
		t.Fatalf("affinity = %+v, want node affinity and pod anti-affinity", affinity)
	}
}

func labelsSet(m map[string]string) labelsMap { return labelsMap(m) }

// labelsMap adapts a map to the labels.Labels a selector matches against.
type labelsMap map[string]string

func (l labelsMap) Has(k string) bool   { _, ok := l[k]; return ok }
func (l labelsMap) Get(k string) string { return l[k] }
func (l labelsMap) Lookup(k string) (string, bool) {
	v, ok := l[k]
	return v, ok
}

// A record written by the takeover while the template update is in flight is
// not lost: the update writes the template and its hash and nothing else.
func TestUpdateMDSKeepsTheActiveRecordAndTheFinalizer(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	_, current := mdsObjects(t, d)
	old := current.DeepCopy()
	old.Annotations[MDSTemplateHashAnnotation] = "0ld0ld0ld0ld0ld0"
	old.Spec.Template.Spec.Containers[0].Image = "public.ecr.aws/simply-block/spdkcsi:pnfs-mds-old"
	old.Finalizers = []string{mdsstate.StateVolumeFinalizer}
	old.Annotations[mdsstate.StateVolumeAnnotation] = "cluster:pool:vol"
	old.Annotations[mdsstate.EpochAnnotation] = "3"
	old.Spec.Replicas = ptr.To(int32(3))

	c := fake.NewClientBuilder().WithScheme(reconcilerScheme(t)).WithObjects(old).Build()
	var stale appsv1.StatefulSet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(old), &stale); err != nil {
		t.Fatal(err)
	}
	// The takeover moves the record after the update read the object.
	var live appsv1.StatefulSet
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(old), &live)
	live.Annotations[mdsstate.EpochAnnotation] = "7"
	live.Annotations[mdsstate.ActiveAnnotation] = old.Name + "-1"
	if err := c.Update(context.Background(), &live); err != nil {
		t.Fatal(err)
	}

	updated, err := UpdateMDS(context.Background(), c, d, &stale, nil)
	if err != nil || !updated {
		t.Fatalf("UpdateMDS = %v, %v, want an update", updated, err)
	}

	var got appsv1.StatefulSet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(old), &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[mdsstate.EpochAnnotation] != "7" || got.Annotations[mdsstate.ActiveAnnotation] != old.Name+"-1" {
		t.Errorf("the record was overwritten: %v", got.Annotations)
	}
	if got.Annotations[mdsstate.StateVolumeAnnotation] != "cluster:pool:vol" {
		t.Errorf("the state volume handle was lost: %v", got.Annotations)
	}
	if !slices.Contains(got.Finalizers, mdsstate.StateVolumeFinalizer) {
		t.Errorf("finalizers = %v, want the state volume's kept", got.Finalizers)
	}
	if got.Annotations[MDSTemplateHashAnnotation] != current.Annotations[MDSTemplateHashAnnotation] ||
		got.Spec.Template.Spec.Containers[0].Image != current.Spec.Template.Spec.Containers[0].Image {
		t.Error("the template and its hash were not updated")
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 3 {
		t.Errorf("replicas = %v, want the existing 3 left to the scale step", got.Spec.Replicas)
	}
}

var _ = apierrors.IsNotFound
var _ = simplyblockv1alpha2.GroupVersion
