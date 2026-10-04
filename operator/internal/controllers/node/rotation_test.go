// What makes the storage-node pods restart when their certificate changes.
//
// The serving certificate is mounted from a Secret, and a mounted Secret is
// updated in place: the file under the pod changes and the process holding it
// keeps serving the old one. Nothing about that is visible from the DaemonSet,
// whose template is identical before and after the rotation, so the pods are
// never rolled and the API keeps presenting an expired certificate.
//
// Stamping the Secret's resourceVersion into the pod template is what turns a
// rotation into a template change, which is the one thing a DaemonSet does roll
// on. It is stamped only where TLS is served, because a deployment that serves
// plaintext has no certificate to rotate.

package node

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// aServingSecret is the storage-node API's certificate, as cert-manager or the
// operator's own rotator writes it.
func aServingSecret() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      utils.SecretNameStorageNodeSetAPITLS,
		Namespace: enrollNamespace,
	}}
}

// templateRevision is what the pod template says the certificate was at.
func templateRevision(t *testing.T, apiClient client.Client) string {
	t.Helper()
	var daemonSet appsv1.DaemonSet
	var sets appsv1.DaemonSetList
	if err := apiClient.List(context.Background(), &sets,
		client.InNamespace(enrollNamespace)); err != nil {
		t.Fatalf("listing the storage-node workload: %v", err)
	}
	if len(sets.Items) != 1 {
		t.Fatalf("%d DaemonSets were written, want the cluster's one", len(sets.Items))
	}
	daemonSet = sets.Items[0]
	return daemonSet.Spec.Template.Annotations[utils.AnnotationTLSSecretRevision]
}

// A rotation changes the pod template, which is what a DaemonSet rolls on.
func TestARotatedCertificateRollsTheStoragePods(t *testing.T) {
	secret := aServingSecret()
	r := aWorkloadReconciler(t, aSizedCluster(), secret)
	r.TLSEnabled = true

	if err := r.reconcileDaemonSet(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("writing the workload: %v", err)
	}
	before := templateRevision(t, r.Client)
	if before == "" {
		t.Fatal("the template records no certificate revision, so a rotation rolls nothing")
	}

	// Rewriting the Secret is what a rotation is, and it is the only change.
	var rotated corev1.Secret
	key := client.ObjectKeyFromObject(secret)
	if err := r.Get(context.Background(), key, &rotated); err != nil {
		t.Fatalf("reading the certificate: %v", err)
	}
	rotated.Data = map[string][]byte{"tls.crt": []byte("a fresh certificate")}
	if err := r.Update(context.Background(), &rotated); err != nil {
		t.Fatalf("rotating the certificate: %v", err)
	}

	if err := r.reconcileDaemonSet(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("writing the workload: %v", err)
	}
	if after := templateRevision(t, r.Client); after == before {
		t.Errorf("the template still records %q, so the pods keep serving the old certificate",
			after)
	}
}

// A deployment that serves plaintext has no certificate to rotate, so nothing is
// stamped and no pod is rolled by the absence.
func TestWithoutTLSNothingIsStampedOnTheTemplate(t *testing.T) {
	r := aWorkloadReconciler(t, aSizedCluster(), aServingSecret())

	if err := r.reconcileDaemonSet(context.Background(), aSizedCluster()); err != nil {
		t.Fatalf("writing the workload: %v", err)
	}

	if revision := templateRevision(t, r.Client); revision != "" {
		t.Errorf("the template records certificate revision %q on a deployment that serves "+
			"plaintext", revision)
	}
}

// A cluster that states no image takes the ControlPlane's, so a deployment
// states its version once. A cluster that has neither is an error rather than a
// workload written with an empty image, which schedules pods that cannot start.
func TestAWorkloadWithNoImageAnywhereIsRefused(t *testing.T) {
	r := aWorkloadReconciler(t, aSizedCluster())
	cluster := aSizedCluster()
	cluster.Spec.StorageNodes.Image = ""

	err := r.reconcileDaemonSet(context.Background(), cluster)

	if err == nil {
		t.Error("a workload was written with no image for its containers")
	}
}
