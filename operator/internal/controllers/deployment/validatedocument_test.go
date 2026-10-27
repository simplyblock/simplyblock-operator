// ValidateDocument answers with the findings the controller raises as events,
// for a caller that holds no reconciler.

package deployment

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// draftNaming is a draft whose one group names the workers given and one NVMe
// device each.
func draftNaming(workers ...string) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return &simplyblockv1alpha2.ClusterDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "draft", Namespace: opsNamespace},
		Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
			NodeSets: []simplyblockv1alpha2.NodeSet{{
				Name: "discovered",
				Groups: []simplyblockv1alpha2.NodeGroup{{
					Name:          "group-1",
					Workers:       workers,
					MgmtInterface: "eth0",
					Devices:       &simplyblockv1alpha2.DeviceSelection{NVMe: []string{"0000:5e:00.0"}},
				}},
			}},
		},
	}
}

func TestValidateDocumentNamesAWorkerTheClusterDoesNotHave(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(opsScheme(t)).
		WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}).Build()

	found, err := ValidateDocument(context.Background(), c, draftNaming("worker-1", "worker-9"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	var missing *Finding
	for i := range found {
		if found[i].Reason == WorkerNotFound {
			missing = &found[i]
		}
	}
	if missing == nil {
		t.Fatalf("no %s finding among %+v", WorkerNotFound, found)
	}
	if !strings.Contains(missing.Message, "worker-9") || strings.Contains(missing.Message, "worker-1,") {
		t.Errorf("the finding names the wrong workers: %q", missing.Message)
	}
}

func TestValidateDocumentFindsNothingWrongWithAValidDraft(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(opsScheme(t)).
		WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}).Build()

	found, err := ValidateDocument(context.Background(), c, draftNaming("worker-1"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, f := range found {
		if f.Reason == WorkerNotFound {
			t.Errorf("a worker the cluster has was reported missing: %+v", f)
		}
	}
}
