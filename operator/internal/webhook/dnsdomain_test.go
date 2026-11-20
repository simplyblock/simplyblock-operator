// Tests that the conversion webhook's self-signed certificate names the
// service under the cluster DNS domain.
package webhook

import (
	"crypto/x509"
	"encoding/pem"
	"slices"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func TestTheSelfSignedCertificateNamesTheClusterDNSDomain(t *testing.T) {
	if err := utils.SetClusterDNSDomain("acme.local"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = utils.SetClusterDNSDomain("") })

	material, err := issueSelfSignedMaterial("svc.simplyblock.svc")
	if err != nil {
		t.Fatalf("issueSelfSignedMaterial: %v", err)
	}
	block, _ := pem.Decode(material.cert)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if !slices.Contains(cert.DNSNames, "svc.simplyblock.svc.acme.local") {
		t.Errorf("DNSNames %v lack the name under acme.local", cert.DNSNames)
	}
	if slices.Contains(cert.DNSNames, "svc.simplyblock.svc.cluster.local") {
		t.Errorf("DNSNames %v carry cluster.local on a cluster whose domain is acme.local", cert.DNSNames)
	}
}
