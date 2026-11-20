// Which handle a stage resolves a volume from (design-superclusters.md §8.3):
// a PersistentVolume a Relocate moved carries the handle of the volume it was
// put on, and the stage starts there rather than at the PV's own handle,
// whose source the move deleted, possibly in a member that is gone.

package node

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/kube"
)

const (
	stagedHandle = "11111111-1111-1111-1111-111111111111:" +
		"22222222-2222-2222-2222-222222222222:33333333-3333-3333-3333-333333333333"
	markHandle = "44444444-4444-4444-4444-444444444444:" +
		"55555555-5555-5555-5555-555555555555:66666666-6666-6666-6666-666666666666"
)

func pvWith(annotations map[string]string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Annotations: annotations}}
}

// U-192, the node's half.
func TestAStageResolvesAMarkedVolumeFromItsMark(t *testing.T) {
	for _, tc := range []struct {
		name string
		pv   *corev1.PersistentVolume
		want string
	}{
		{"a marked volume starts at its mark", pvWith(map[string]string{kube.AnnoRelocatedTo: markHandle}), markHandle},
		{"an unmarked volume starts at its own handle", pvWith(nil), stagedHandle},
		{"no PersistentVolume found starts at the handle", nil, stagedHandle},
		{"an unparsable mark is ignored", pvWith(map[string]string{kube.AnnoRelocatedTo: "garbage"}), stagedHandle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolutionStart(stagedHandle, tc.pv); got != tc.want {
				t.Errorf("resolutionStart = %q, want %q", got, tc.want)
			}
		})
	}
}
