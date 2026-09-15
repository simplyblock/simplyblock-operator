// What an external control plane is reached with: the endpoint, the token, and
// the CA its certificate is verified against.
//
// The CA is the one of the three that was declared in the API and wired to
// nothing, so a probe fell back to the system trust store and an endpoint signed
// by a private CA could not be reached at all. These pin that the field reaches
// the transport.

package controlplane

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// A self-signed certificate, used only to check that a PEM bundle reaches the
// transport's root pool. Nothing connects to it.
const testCAPEM = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----
`

func externalWithCABundle(t *testing.T, secretName string, data map[string][]byte) (
	*simplyblockv1alpha2.ControlPlane, []client.Object,
) {
	t.Helper()
	cp := externalControlPlane("https://sb-control.example.com:5000")
	cp.Spec.Source.External.CredentialsSecretRef = nil
	cp.Spec.Source.External.CABundleSecretRef = &corev1.LocalObjectReference{Name: secretName}

	var extra []client.Object
	if data != nil {
		extra = append(extra, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNamespace},
			Data:       data,
		})
	}
	return cp, extra
}

// A named CA bundle produces a client that verifies against it, rather than the
// nil client that means the system trust store.
func TestACABundleReachesTheTransport(t *testing.T) {
	cp, extra := externalWithCABundle(t, "cp-ca", map[string][]byte{"ca.crt": []byte(testCAPEM)})
	objects := append([]client.Object{cp}, extra...)

	access, err := resolveExternal(context.Background(), newClient(t, objects...), cp)
	if err != nil {
		t.Fatalf("resolveExternal: %v", err)
	}
	if access.client == nil {
		t.Fatal("no HTTP client was built, so the probe would use the system trust store " +
			"and an endpoint signed by this CA could not be reached")
	}
	if access.client.Transport == nil {
		t.Fatal("the client carries no transport, so the root pool went nowhere")
	}
}

// No reference means the system trust store, which is a nil client rather than
// an empty pool: an empty pool verifies nothing at all.
func TestNoCABundleLeavesTheSystemTrustStore(t *testing.T) {
	cp := externalControlPlane("https://sb-control.example.com:5000")
	cp.Spec.Source.External.CredentialsSecretRef = nil

	access, err := resolveExternal(context.Background(), newClient(t, cp), cp)
	if err != nil {
		t.Fatalf("resolveExternal: %v", err)
	}
	if access.client != nil {
		t.Error("a client was built although no CA bundle was named")
	}
}

// A CA bundle that is named and unusable is an error rather than a fall back.
// Naming one says the endpoint is signed by it, so verifying against something
// else would connect to a control plane nobody vouched for.
func TestAnUnusableCABundleIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string][]byte
		want string
	}{
		{"the Secret does not exist", nil, "does not exist"},
		{"it carries no CA key", map[string][]byte{"other": []byte(testCAPEM)}, "no ca.crt or tls.crt key"},
		{"it carries no PEM", map[string][]byte{"ca.crt": []byte("not a certificate")}, "no PEM certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp, extra := externalWithCABundle(t, "cp-ca", tc.data)
			objects := append([]client.Object{cp}, extra...)

			_, err := resolveExternal(context.Background(), newClient(t, objects...), cp)
			if err == nil {
				t.Fatal("an unusable CA bundle was accepted")
			}
			var credentials *credentialsError
			if !errorsAs(err, &credentials) {
				t.Fatalf("err = %v, want a credentials error", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
