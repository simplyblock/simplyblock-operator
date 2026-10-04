// Tests for the annotation-spelling check. The inventory's own tests cover the
// matching, so what is left is that the check reaches the objects that carry
// these keys, which includes claims in namespaces the installation does not own.

package check

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// claim is a PersistentVolumeClaim carrying whatever annotations a test needs.
func claim(namespace, name string, annotations map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: annotations},
	}
}

func spellings(t *testing.T, scope *upgrade.Scope) upgrade.Findings {
	t.Helper()

	findings, err := AnnotationSpellings().Check(t.Context(), scope)
	if err != nil {
		t.Fatalf("AnnotationSpellings: %v", err)
	}
	return findings
}

func TestSpellings_AClaimWithTwoDisagreeingSpellingsIsAnError(t *testing.T) {
	scope := clusterWideScope(t, claim("team-a", "data", map[string]string{
		"simplyblock.io/backup-policy":         "nightly",
		"storage.simplyblock.io/backup-policy": "hourly",
	}))

	findings := spellings(t, scope)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1:\n%v", len(findings), findings)
	}

	rendered := findings[0].String()
	for _, want := range []string{
		"PersistentVolumeClaim team-a/data",
		"simplyblock.io/backup-policy = nightly",
		"storage.simplyblock.io/backup-policy = hourly",
		"cannot choose between two values",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the finding does not carry %q:\n%s", want, rendered)
		}
	}
}

func TestSpellings_AClaimCarryingOneSpellingIsFine(t *testing.T) {
	// Every claim on every cluster that has not been migrated looks like this,
	// so reporting it would fail every preflight ever run.
	scope := clusterWideScope(t, claim("team-a", "data", map[string]string{
		"simplyblock.io/backup-policy": "nightly",
	}))

	if findings := spellings(t, scope); len(findings) != 0 {
		t.Fatalf("an unmigrated claim produced %d findings:\n%v", len(findings), findings)
	}
}

func TestSpellings_AClaimWhoseSpellingsAgreeIsFine(t *testing.T) {
	// A claim the rewrite has already reached. Reporting it would make the
	// rewrite fail its own second run, which §22 requires be safe.
	scope := clusterWideScope(t, claim("team-a", "data", map[string]string{
		"simplyblock.io/backup-policy":         "nightly",
		"storage.simplyblock.io/backup-policy": "nightly",
	}))

	if findings := spellings(t, scope); len(findings) != 0 {
		t.Fatalf("an already migrated claim produced %d findings:\n%v", len(findings), findings)
	}
}

func TestSpellings_ReachesClaimsOutsideTheInstallationNamespace(t *testing.T) {
	// A claim lives where its workload does. A check that only read the
	// installation's namespace would miss almost every one of them.
	scope := clusterWideScope(t,
		claim("team-a", "data", map[string]string{
			"simplyblock.io/selected-storage-node":         "node-1",
			"storage.simplyblock.io/selected-storage-node": "node-2",
		}),
		claim("team-b", "logs", map[string]string{
			"simplyblock.io/guardian-disable":         "true",
			"storage.simplyblock.io/guardian-disable": "false",
		}),
	)

	if findings := spellings(t, scope); len(findings) != 2 {
		t.Fatalf("got %d findings, want one per claim:\n%v", len(findings), findings)
	}
}

func TestSpellings_ReachesTheInstallationGraphToo(t *testing.T) {
	// The custom resources are in the other graph, and trigger-realignment
	// sits on a StorageCluster.
	scope := clusterWideScope(t)
	scope.Adopt(&simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-a", Namespace: "simplyblock",
			Annotations: map[string]string{
				"simplyblock.io/trigger-realignment":         "true",
				"storage.simplyblock.io/trigger-realignment": "false",
			},
		},
	})

	if findings := spellings(t, scope); len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: the installation's own objects carry "+
			"these keys as well:\n%v", len(findings), findings)
	}
}

func TestSpellings_FindsAConflictInALabelAsWellAsAnAnnotation(t *testing.T) {
	scope := clusterWideScope(t)
	scope.Adopt(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "worker-1",
		Labels: map[string]string{
			"simplyblock.io/storage":         "yes",
			"storage.simplyblock.io/storage": "no",
		},
	}})

	if findings := spellings(t, scope); len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: these keys are labels as often as "+
			"annotations:\n%v", len(findings), findings)
	}
}

func TestSpellings_AnEmptyClusterProducesNothing(t *testing.T) {
	if findings := spellings(t, clusterWideScope(t)); len(findings) != 0 {
		t.Fatalf("an empty cluster produced %d findings:\n%v", len(findings), findings)
	}
}

func TestSpellings_IsDeterministic(t *testing.T) {
	// The graph indexes kinds in a map, so the walk has to be sorted or a user
	// diffing two preflights reads map iteration as a change.
	//
	// The fixture is what makes this test able to fail: the objects are of
	// several kinds and they carry the same key, so the order the kinds are
	// walked in is the order the findings come out in. Objects of one kind, or
	// of several kinds carrying different keys, are ordered by the inventory
	// instead and would pass whether the walk is sorted or not.
	disagree := map[string]string{
		"simplyblock.io/backup-policy":         "nightly",
		"storage.simplyblock.io/backup-policy": "hourly",
	}

	scope := clusterWideScope(t,
		claim("team-a", "data", disagree),
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{
			Name: "pv-1", Annotations: disagree,
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "pod-1", Namespace: "team-a", Annotations: disagree,
		}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "s-1", Namespace: "team-a", Annotations: disagree,
		}},
	)

	first := spellings(t, scope)
	if len(first) != 4 {
		t.Fatalf("got %d findings, want one per object:\n%v", len(first), first)
	}
	for range 20 {
		if got := spellings(t, scope).String(); got != first.String() {
			t.Fatalf("two runs reported different things:\n%s\n---\n%s", first, got)
		}
	}
}
