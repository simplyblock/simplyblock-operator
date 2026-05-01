package controller

import (
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func lvol(ns, name, lvolID string) simplyblockv1alpha1.AttachedLvol {
	return simplyblockv1alpha1.AttachedLvol{PVCNamespace: ns, PVCName: name, LvolID: lvolID}
}

func TestDiffAttachments_NoChange(t *testing.T) {
	a := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	b := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	if got := diffAttachments(a, b); len(got) != 0 {
		t.Fatalf("expected no diff, got %v", got)
	}
}

func TestDiffAttachments_NewAttachment(t *testing.T) {
	desired := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-aaa"),
		lvol("ns", "pvc2", "lvol-bbb"),
	}
	current := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	got := diffAttachments(desired, current)
	if len(got) != 1 || got[0].PVCName != "pvc2" {
		t.Fatalf("expected pvc2 to attach, got %v", got)
	}
}

func TestDiffAttachments_RemovedAttachment(t *testing.T) {
	desired := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	current := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-aaa"),
		lvol("ns", "pvc2", "lvol-bbb"),
	}
	got := diffAttachments(current, desired)
	if len(got) != 1 || got[0].PVCName != "pvc2" {
		t.Fatalf("expected pvc2 to detach, got %v", got)
	}
}

// TestDiffAttachments_ReboundPVC is the regression test for the bug where
// a PVC rebound to a new lvol was invisible to the diff (same ns/name, different
// lvolID). The old lvol must appear in toDetach and the new one in toAttach.
func TestDiffAttachments_ReboundPVC(t *testing.T) {
	desired := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-new")}
	current := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-old")}

	toAttach := diffAttachments(desired, current)
	toDetach := diffAttachments(current, desired)

	if len(toAttach) != 1 || toAttach[0].LvolID != "lvol-new" {
		t.Fatalf("expected lvol-new in toAttach, got %v", toAttach)
	}
	if len(toDetach) != 1 || toDetach[0].LvolID != "lvol-old" {
		t.Fatalf("expected lvol-old in toDetach, got %v", toDetach)
	}
}

func TestRemoveAttachment(t *testing.T) {
	slice := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-aaa"),
		lvol("ns", "pvc2", "lvol-bbb"),
	}
	result := removeAttachment(slice, lvol("ns", "pvc1", "lvol-aaa"))
	if len(result) != 1 || result[0].PVCName != "pvc2" {
		t.Fatalf("expected only pvc2 remaining, got %v", result)
	}
}

// Removing by PVC key alone must not drop an entry that shares the name but has
// a different lvolID (e.g. after a rebind, the new attachment must survive).
func TestRemoveAttachment_DoesNotMatchDifferentLvol(t *testing.T) {
	slice := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-new"),
	}
	result := removeAttachment(slice, lvol("ns", "pvc1", "lvol-old"))
	if len(result) != 1 {
		t.Fatalf("expected new attachment to survive removal of old lvolID, got %v", result)
	}
}
