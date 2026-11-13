// Tests for the libfdb_c the FoundationDB operator runs with.
package controlplane

import (
	"slices"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// fdbOperatorImageWithOldClient is the operator image last confirmed to link
// libfdb_c 7.1.
const fdbOperatorImageWithOldClient = "quay.io/simplyblock-io/fdb-kubernetes-operator:v2.13.0"

// Regression: 2026-10-28-fdb-operator-7-1-client — the FoundationDB operator
// image links libfdb_c 7.1.67. When no coordinator name resolves, creating a
// database fails, and 7.1 segfaults in fdb_run_network while tearing the failed
// database down (apple/foundationdb#12166, fixed in 7.2 by #8115). The
// deployment mounts the init container's newer library over it.
//
// A new fdbOperatorImage fails this test until someone checks whether the
// mount is still needed:
//
//	podman run --rm --entrypoint rpm <fdbOperatorImage> -qf /usr/lib64/libfdb_c.so
//
// 7.2 or newer: delete the mount, clientLibrary's use in it, and this file.
// Still 7.1: set fdbOperatorImageWithOldClient to the new image.
// No such file: the image moved the library and the mount no longer covers it.
func TestTheDatabaseOperatorHackIsReviewedOnImageBump(t *testing.T) {
	if fdbOperatorImage != fdbOperatorImageWithOldClient {
		t.Fatalf("fdbOperatorImage is %s, the libfdb_c mount was verified against %s: "+
			"check whether the new image still ships libfdb_c 7.1 (see this test's comment)",
			fdbOperatorImage, fdbOperatorImageWithOldClient)
	}
}

// The mounted library is the file the init container writes. A version change
// on one side only would mount a path the init container never created.
func TestTheDatabaseOperatorMountsTheLibraryItsInitContainerCopies(t *testing.T) {
	spec := fdbOperatorDeployment(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{})).Spec.Template.Spec

	args := spec.InitContainers[0].Args
	i := slices.Index(args, "--copy-library")
	if i < 0 || i+1 >= len(args) {
		t.Fatal("the init container copies no client library")
	}
	want := "libfdb_c_" + args[i+1] + ".so"

	for _, m := range spec.Containers[0].VolumeMounts {
		if m.MountPath == "/usr/lib64/libfdb_c.so" {
			if m.SubPath != want {
				t.Fatalf("libfdb_c mount reads %q, the init container writes %q", m.SubPath, want)
			}
			return
		}
	}
	t.Fatal("the database operator runs the libfdb_c its image ships")
}
