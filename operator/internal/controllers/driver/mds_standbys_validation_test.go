// spec.pnfs.mds.standbys as the apiserver sees it: the default a chart install
// relies on and the bounds of design-pnfs-mds-standby.md §4.1 (O-34).

package driver

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func driverWithStandbys(standbys *int32) *simplyblockv1alpha2.SimplyblockDriver {
	return &simplyblockv1alpha2.SimplyblockDriver{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "standbys-", Namespace: "default"},
		Spec: simplyblockv1alpha2.SimplyblockDriverSpec{
			PNFS: simplyblockv1alpha2.DriverPNFS{MDS: &simplyblockv1alpha2.DriverPNFSMDS{Standbys: standbys}},
		},
	}
}

// O-34: 0 to 4 are accepted and 5 and -1 are rejected.
func TestStandbysAcceptsZeroToFourAndNothingElse(t *testing.T) {
	c := apiServer(t)
	for _, tc := range []struct {
		standbys int32
		denied   bool
	}{{0, false}, {1, false}, {4, false}, {5, true}, {-1, true}} {
		d := driverWithStandbys(ptr.To(tc.standbys))
		err := c.Create(context.Background(), d)
		switch {
		case tc.denied && err == nil:
			t.Errorf("standbys %d was accepted", tc.standbys)
			_ = c.Delete(context.Background(), d)
		case !tc.denied && err != nil:
			t.Errorf("standbys %d was rejected: %v", tc.standbys, err)
		case !tc.denied:
			_ = c.Delete(context.Background(), d)
		}
	}
}

// An object that says nothing runs one standby, which the chart's `mds: {}`
// relies on.
func TestStandbysDefaultsToOne(t *testing.T) {
	c := apiServer(t)
	d := driverWithStandbys(nil)
	if err := c.Create(context.Background(), d); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = c.Delete(context.Background(), d) }()
	if got := d.Spec.PNFS.MDS.Standbys; got == nil || *got != 1 {
		t.Errorf("standbys = %v, want the default 1", got)
	}
}
