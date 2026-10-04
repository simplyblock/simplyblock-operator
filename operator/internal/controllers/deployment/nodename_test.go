// The limit that binds a StorageNode's derived name.
//
// §19.1 of design-api-upgrade.md states the rule these cases hold the formula
// to: where a name is copied into a label, the label's 63 bytes bind it and not
// the 253 an object name may be. A StorageNode's name is copied into
// storage.simplyblock.io/node on every StorageDevice the mirror writes, so the
// name is a label value that happens to also be an object name.
//
// The worker names are real ones rather than strings of a chosen length,
// because the question the rule answers is whether ordinary inputs overflow,
// and a cloud that names a worker after its fully qualified domain name spends
// forty-two of the sixty-three before the cluster name is in it.

package deployment

import (
	"testing"

	"github.com/simplyblock/atlas/kube"
)

func TestANodeNameFitsTheLabelItIsCopiedInto(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster string
		worker  string
		slot    int32
	}{
		{
			name:    "a short cluster on a bare hostname",
			cluster: "production",
			worker:  "worker-3",
		},
		{
			name:    "a regional cluster on an EKS worker",
			cluster: "production-eu-central-1",
			worker:  "ip-10-0-1-23.eu-central-1.compute.internal",
			slot:    1,
		},
		{
			name:    "a GKE worker, which carries the cluster name in its own",
			cluster: "storage",
			worker:  "gke-production-eu-central-1-storage-pool-7f3a2b1c-k4nd",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			derived := nodeName(tc.cluster, tc.worker, tc.slot)

			if errs := kube.Validate(kube.LabelValue, derived); len(errs) > 0 {
				t.Errorf("the name %q (%d bytes) is not a legal label value, so writing it "+
					"into storage.simplyblock.io/node is a StorageDevice reconcile that "+
					"retries forever: %v", derived, len(derived), errs)
			}
		})
	}
}
