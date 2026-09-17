// The one thing about spec.pnfs that admission decides rather than the workload
// builders: pNFS cannot work without csi-link.
//
// It is a CEL rule on the type rather than a check in the reconciler because
// reconciling cannot recover from it. The operator drives export assembly by
// calling the MDS host over the link, so with no link there is no call, and
// every ReadWriteMany claim on the cluster parks in Pending with nothing to
// wait for and no message saying why. Admission can say it at the moment the
// mistake is made.

package driver

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestPNFSRequiresTheLink(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name       string
		link       *bool
		pnfs       *bool
		wantDenied bool
	}{
		{name: "both unset"},
		{name: "both false", link: ptr.To(false), pnfs: ptr.To(false)},
		{name: "link alone", link: ptr.To(true), pnfs: ptr.To(false)},
		{name: "both true", link: ptr.To(true), pnfs: ptr.To(true)},
		{name: "pnfs alone", link: ptr.To(false), pnfs: ptr.To(true), wantDenied: true},
		{name: "pnfs with an unset link", pnfs: ptr.To(true), wantDenied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &simplyblockv1alpha2.SimplyblockDriver{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "pnfs-", Namespace: "default"},
				Spec: simplyblockv1alpha2.SimplyblockDriverSpec{
					Link: simplyblockv1alpha2.DriverLink{EnableLink: tc.link},
					PNFS: simplyblockv1alpha2.DriverPNFS{EnablePNFS: tc.pnfs},
				},
			}
			err := apiClient.Create(context.Background(), d)
			if err == nil {
				t.Cleanup(func() { _ = apiClient.Delete(context.Background(), d) })
			}
			switch {
			case !tc.wantDenied && err != nil:
				t.Fatalf("a valid driver was refused: %v", err)
			case tc.wantDenied && err == nil:
				t.Fatal("pNFS was admitted with no link to drive it")
			case tc.wantDenied && !errors.IsInvalid(err):
				t.Fatalf("refused for the wrong reason: %v", err)
			case tc.wantDenied && !strings.Contains(err.Error(), "csi-link"):
				t.Errorf("the rejection does not name the link: %v", err)
			}
		})
	}
}
