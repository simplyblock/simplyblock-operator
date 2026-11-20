// Admission of a Relocate (design-superclusters.md §8.1, §9.3, §9.4): the
// target is another eligible member of the volume's own supercluster, and the
// volume is one a move can carry.

package webhook

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	relocateSupercluster = "prod"
	relocateSource       = "prod-w1-lid"
	relocateTarget       = "prod-w2-nrh"
	relocateTargetUUID   = "44444444-4444-4444-4444-444444444444"
	relocateClaim        = "data"
	relocateClaimNS      = "app"
)

func relocateOperation(target string) *simplyblockv1alpha2.PersistentVolumeOps {
	return &simplyblockv1alpha2.PersistentVolumeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "relocate-1"},
		Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
			PersistentVolumeName: pvopsVolume,
			Action:               simplyblockv1alpha2.PersistentVolumeOpsActionRelocate,
			Relocate:             &simplyblockv1alpha2.RelocateVolumeSpec{TargetClusterRef: target},
		},
	}
}

func member(name, uuid, supercluster string, edit ...func(*simplyblockv1alpha2.StorageCluster)) *simplyblockv1alpha2.StorageCluster {
	c := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pvopsNS},
		Spec:       simplyblockv1alpha2.StorageClusterSpec{SuperclusterRef: supercluster},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: uuid},
	}
	for _, e := range edit {
		e(c)
	}
	return c
}

// relocatableVolume is the volume of a claim in member prod-w1-lid.
func relocatableVolume(edit ...func(*corev1.PersistentVolume)) *corev1.PersistentVolume {
	pv := simplyblockVolume()
	pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: relocateClaimNS, Name: relocateClaim}
	for _, e := range edit {
		e(pv)
	}
	return pv
}

func relocateWorld(objs ...client.Object) []client.Object {
	return append([]client.Object{
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: relocateClaim, Namespace: relocateClaimNS}},
		member(relocateSupercluster, "55555555-5555-5555-5555-555555555555", ""),
		member(relocateSource, pvopsCluster, relocateSupercluster),
		member(relocateTarget, relocateTargetUUID, relocateSupercluster),
	}, objs...)
}

// U-65
func TestARelocateToAnotherEligibleMemberIsAdmitted(t *testing.T) {
	v := pvopsValidator(t, relocateWorld(relocatableVolume())...)

	if got := v.Handle(context.Background(), pvopsCreateRequest(t, relocateOperation(relocateTarget))); !got.Allowed {
		t.Fatalf("a well-formed Relocate was refused: %s", got.Result.Message)
	}
}

// U-66 to U-71, each the well-formed request broken in one way.
func TestARelocateNoReconcileCouldCarryOutIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		world  []client.Object
		want   string
	}{
		{
			name: "U-66 the volume's own member", target: relocateSource,
			world: relocateWorld(relocatableVolume()), want: "already in member " + relocateSource,
		},
		{
			name: "U-67 a member of another supercluster", target: "dev-w1-abc",
			world: relocateWorld(relocatableVolume(), member("dev-w1-abc", "66666666-6666-6666-6666-666666666666", "dev")),
			want:  "member of supercluster dev",
		},
		{
			name: "a target that does not exist", target: "prod-w9-zzz",
			world: relocateWorld(relocatableVolume()), want: "no StorageCluster " + pvopsNS + "/prod-w9-zzz",
		},
		{
			name: "U-68 a volume in a standalone cluster", target: relocateTarget,
			world: []client.Object{
				relocatableVolume(),
				&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: relocateClaim, Namespace: relocateClaimNS}},
				member(relocateSource, pvopsCluster, ""),
				member(relocateTarget, relocateTargetUUID, relocateSupercluster),
			},
			want: "is not a member of a supercluster",
		},
		{
			name: "U-69 a target with placement disabled", target: relocateTarget,
			world: []client.Object{
				relocatableVolume(),
				&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: relocateClaim, Namespace: relocateClaimNS}},
				member(relocateSource, pvopsCluster, relocateSupercluster),
				member(relocateTarget, relocateTargetUUID, relocateSupercluster, func(c *simplyblockv1alpha2.StorageCluster) {
					c.Spec.DisablePlacement = ptr.To(true)
				}),
			},
			want: "disablePlacement",
		},
		{
			name: "U-70 a volume in a consistency group", target: relocateTarget,
			world: []client.Object{
				relocatableVolume(),
				&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
					Name: relocateClaim, Namespace: relocateClaimNS,
					Labels: map[string]string{"storage.simplyblock.io/consistency-group": "orders"},
				}},
				member(relocateSource, pvopsCluster, relocateSupercluster),
				member(relocateTarget, relocateTargetUUID, relocateSupercluster),
			},
			want: "consistency group orders",
		},
		{
			name: "U-71 an encrypted volume", target: relocateTarget,
			world: relocateWorld(relocatableVolume(func(pv *corev1.PersistentVolume) {
				pv.Spec.CSI.VolumeAttributes = map[string]string{"encryption": "true"}
			})),
			want: "EncryptedVolumeNotMovable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := pvopsValidator(t, tc.world...)

			got := v.Handle(context.Background(), pvopsCreateRequest(t, relocateOperation(tc.target)))

			if got.Allowed {
				t.Fatal("the Relocate was admitted")
			}
			if !strings.Contains(got.Result.Message, tc.want) {
				t.Errorf("refused with %q, want it to say %q", got.Result.Message, tc.want)
			}
		})
	}
}
