// Tests for the operator's own client certificate on an install no chart made:
// issued with the issuer chain, and mounted where the operator reads it.

package prerequisites

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/simplyblock/simplyblock-operator/internal/tlsutil"
)

// The chain issues the client certificate the operator presents for mutual
// TLS, which only the chart issued before.
func TestTheIssuerChainIssuesTheOperatorClientCertificate(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	if err := EnsureCertManagerIssuerChain(context.Background(), c, "simplyblock"); err != nil {
		t.Fatal(err)
	}
	cert := get(t, c, certificateGVK, "simplyblock", operatorClientCertificateName)
	if cert == nil {
		t.Fatal("the operator's client Certificate was not created")
	}
	if s, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName"); s != OperatorClientSecretName {
		t.Errorf("secretName = %q, want %q", s, OperatorClientSecretName)
	}
	usages, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "usages")
	if !slices.Contains(usages, "client auth") {
		t.Errorf("usages = %v, want client auth", usages)
	}
}

// The Deployment OLM installs mounts that Secret, optional so the pod starts
// before cert-manager issues it, at the directory the operator reads its TLS
// material from.
func TestTheBundleDeploymentMountsTheOperatorClientCertificate(t *testing.T) {
	raw, err := os.ReadFile("../../config/manager/manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var d appsv1.Deployment
	for _, doc := range splitYAML(raw) {
		var probe struct{ Kind string }
		if yaml.Unmarshal(doc, &probe) == nil && probe.Kind == kindDeployment {
			if err := yaml.Unmarshal(doc, &d); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := filepath.Dir(tlsutil.ServiceCABundlePath)
	var volume string
	for _, m := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == want {
			volume = m.Name
		}
	}
	if volume == "" {
		t.Fatalf("nothing is mounted at %s", want)
	}
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name != volume {
			continue
		}
		if v.Secret == nil || v.Secret.SecretName != OperatorClientSecretName {
			t.Fatalf("volume %s is %+v, want Secret %s", volume, v.VolumeSource, OperatorClientSecretName)
		}
		if v.Secret.Optional == nil || !*v.Secret.Optional {
			t.Error("the Secret volume is not optional, so the operator could not start before it is issued")
		}
		return
	}
	t.Fatalf("volume %s is mounted but not declared", volume)
}

func splitYAML(raw []byte) [][]byte {
	parts := strings.Split(string(raw), "\n---")
	out := make([][]byte, 0, len(parts))
	for _, part := range parts {
		out = append(out, []byte(part))
	}
	return out
}

// The bundle's operator is replaced rather than rolled. The self-signed webhook
// certificate is written by the leader alone and readiness waits for it, so a
// rolling update's new pod never becomes ready while the old pod holds the lease.
func TestTheBundleDeploymentIsRecreatedOnUpgrade(t *testing.T) {
	raw, err := os.ReadFile("../../config/manager/manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range splitYAML(raw) {
		var d appsv1.Deployment
		if yaml.Unmarshal(doc, &d) != nil || d.Kind != kindDeployment {
			continue
		}
		if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
			t.Errorf("strategy = %q, want Recreate", d.Spec.Strategy.Type)
		}
		return
	}
	t.Fatal("no Deployment in config/manager/manager.yaml")
}

// The chain issues the CSI link's serving certificate, which the chart cannot
// render on a cluster that serves no cert-manager API until the operator has
// installed it.
func TestTheIssuerChainIssuesTheCSILinkServingCertificate(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	if err := EnsureCertManagerIssuerChain(context.Background(), c, "simplyblock"); err != nil {
		t.Fatal(err)
	}
	cert := get(t, c, certificateGVK, "simplyblock", "simplyblock-csi-link")
	if cert == nil {
		t.Fatal("the CSI link's serving Certificate was not created")
	}
	if s, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName"); s != "simplyblock-csi-link-tls" {
		t.Errorf("secretName = %q, want simplyblock-csi-link-tls", s)
	}
	dnsNames, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	if !slices.Contains(dnsNames, "simplyblock-csi-link.simplyblock.svc") {
		t.Errorf("dnsNames = %v, want simplyblock-csi-link.simplyblock.svc", dnsNames)
	}
	usages, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "usages")
	if !slices.Contains(usages, "server auth") {
		t.Errorf("usages = %v, want server auth", usages)
	}
}
