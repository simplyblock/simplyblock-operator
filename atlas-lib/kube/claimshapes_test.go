// Tests for the claim shapes a StorageClass's parameters allow.

package kube

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

var (
	rwxBlock = ClaimShape{AccessMode: corev1.ReadWriteMany, VolumeMode: corev1.PersistentVolumeBlock}
	rwoBlock = ClaimShape{AccessMode: corev1.ReadWriteOnce, VolumeMode: corev1.PersistentVolumeBlock}
	rwxFS    = ClaimShape{AccessMode: corev1.ReadWriteMany, VolumeMode: corev1.PersistentVolumeFilesystem}
	rwoFS    = ClaimShape{AccessMode: corev1.ReadWriteOnce, VolumeMode: corev1.PersistentVolumeFilesystem}
)

func TestClaimShapesFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]string
		want   []ClaimShape
	}{
		// ReadWriteMany raw block first: it is what a claim that names neither
		// mode gets, and it is what a VM needs to live-migrate.
		{"an ordinary class", map[string]string{ParamPool: "p"}, []ClaimShape{rwxBlock, rwoBlock, rwoFS}},
		{"a stripe width of one is ordinary", map[string]string{ParamStripeWidth: "1"}, []ClaimShape{rwxBlock, rwoBlock, rwoFS}},
		{"encryption is per host", map[string]string{ParamEncryption: "true"}, []ClaimShape{rwxBlock, rwoBlock, rwoFS}},
		{"a striped class", map[string]string{ParamStripeWidth: "3"}, []ClaimShape{rwoBlock, rwoFS}},
		{"client-side compression", map[string]string{ParamClientCompression: "true"}, []ClaimShape{rwoBlock, rwoFS}},
		{"client-side deduplication", map[string]string{ParamClientDeduplication: "true"}, []ClaimShape{rwoBlock, rwoFS}},
		{"a pNFS export", map[string]string{ParamFSType: FSTypePNFS}, []ClaimShape{rwxFS, rwoFS}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClaimShapesFor(tc.params); !slices.Equal(got, tc.want) {
				t.Errorf("ClaimShapesFor(%v) = %v, want %v", tc.params, got, tc.want)
			}
		})
	}
}

func TestHostBound(t *testing.T) {
	for _, tc := range []struct {
		params map[string]string
		bound  bool
	}{
		{map[string]string{}, false},
		{map[string]string{ParamStripeWidth: "1"}, false},
		{map[string]string{ParamStripeWidth: "2"}, true},
		{map[string]string{ParamClientCompression: "true"}, true},
		{map[string]string{ParamClientDeduplication: "true"}, true},
		{map[string]string{ParamClientCompression: "false", ParamClientDeduplication: "false"}, false},
		{map[string]string{ParamFSType: FSTypePNFS}, false},
	} {
		if reason := HostBound(tc.params); (reason != "") != tc.bound {
			t.Errorf("HostBound(%v) = %q, want bound=%v", tc.params, reason, tc.bound)
		}
	}
}

func TestCloneTarget(t *testing.T) {
	if !CanBeCloneTarget(map[string]string{}) {
		t.Error("an ordinary class cannot be a clone target, want it can")
	}
	if CanBeCloneTarget(map[string]string{ParamFSType: FSTypePNFS}) {
		t.Error("a pNFS class can be a clone target, want it cannot: CreateVolume refuses one")
	}
}
