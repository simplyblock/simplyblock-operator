// What the volume context is read as on the way into a plan.
//
// The context is the only thing the node service knows about a volume it did
// not create, so a fact dropped here is a fact the layers below never get. The
// cases are about the reading rather than about the staging: whether the plan
// carries what the context said.

package node

import (
	"testing"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// An encrypted volume is planned as one whose content cannot be read, because
// the crypto bdev sits under the namespace the host connects to: an empty
// encrypted volume decrypts from zeros into pseudo-random plaintext, which the
// content probe reads as somebody else's data. Losing the flag on the way to
// the plan is what refuses every encrypted volume at its first stage.
func TestAnEncryptedVolumeIsPlannedAsUnreadable(t *testing.T) {
	for value, want := range map[string]bool{
		"True":  true,
		"true":  true,
		"1":     true,
		"false": false,
		"":      false,
		"yes":   false,
	} {
		volume := stackVolume("/staging", map[string]string{"encryption": value}, mountCapability())
		if volume.Encrypted != want {
			t.Errorf("encryption=%q planned as Encrypted=%v, want %v", value, volume.Encrypted, want)
		}
	}
}

// A volume context that says nothing about encryption is a plaintext volume,
// which is what every volume staged before the controller forwarded the
// parameter looks like. The strict content guard is the right one for it.
func TestAVolumeContextWithoutTheFlagIsPlaintext(t *testing.T) {
	volume := stackVolume("/staging", map[string]string{}, mountCapability())
	if volume.Encrypted {
		t.Error("a context with no encryption key was planned as encrypted")
	}
}

// The PersistentVolume and the claim the node service was told about travel
// into the plan's volume, from where the volume-group layer writes them into the
// group's LVM metadata as informational tags. A context that names neither
// leaves them empty rather than inventing them.
func TestStackVolumeCarriesThePVAndClaimNames(t *testing.T) {
	vc := map[string]string{
		csicommon.CSIStoragePVNameKey:    "pvc-1234",
		csicommon.CSIStorageNamespaceKey: "team-a",
		csicommon.CSIStorageNameKey:      "data",
	}
	volume := stackVolume("/staging", vc, mountCapability())
	if volume.PVName != "pvc-1234" || volume.PVCNamespace != "team-a" || volume.PVCName != "data" {
		t.Fatalf("volume carries pv=%q pvc=%s/%s, want pvc-1234 and team-a/data",
			volume.PVName, volume.PVCNamespace, volume.PVCName)
	}
	bare := stackVolume("/staging", map[string]string{}, mountCapability())
	if bare.PVName != "" || bare.PVCNamespace != "" || bare.PVCName != "" {
		t.Fatalf("a context naming no objects produced pv=%q pvc=%s/%s", bare.PVName, bare.PVCNamespace, bare.PVCName)
	}
}
