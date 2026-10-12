// Tests that the control-plane install builds its in-cluster names from the
// cluster DNS domain rather than from cluster.local.
package controlplane

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func useClusterDNSDomain(t *testing.T, domain string) {
	t.Helper()
	if err := utils.SetClusterDNSDomain(domain); err != nil {
		t.Fatalf("SetClusterDNSDomain(%q): %v", domain, err)
	}
	t.Cleanup(func() { _ = utils.SetClusterDNSDomain("") })
}

// Regression: 2026-10-12-fdb-coordinators-in-cluster-local — on a cluster whose
// DNS domain is not cluster.local, the FoundationDB operator wrote coordinator
// hostnames under cluster.local into the cluster file, because routing.dnsDomain
// was left to its default. None of them resolved, libfdb_c failed to determine
// its public address, and the FoundationDB operator crashed with a SIGSEGV in
// fdb_run_network on every reconcile.
func TestTheDatabaseNamesItsCoordinatorsUnderTheClusterDNSDomain(t *testing.T) {
	useClusterDNSDomain(t, "acme.local")

	cluster := foundationDBCluster(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))

	got, _, _ := unstructured.NestedString(cluster.Object, "spec", "routing", "dnsDomain")
	if got != "acme.local" {
		t.Fatalf("spec.routing.dnsDomain = %q, want acme.local", got)
	}
}

func TestTheManagementAPIEndpointIsUnderTheClusterDNSDomain(t *testing.T) {
	useClusterDNSDomain(t, "acme.local")

	got := localEndpoint(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))
	if !strings.Contains(got, "."+"simplyblock.svc.acme.local:") {
		t.Fatalf("localEndpoint = %q, not under acme.local", got)
	}
}

func TestThePeerCertificateNamesTheClusterDNSDomain(t *testing.T) {
	useClusterDNSDomain(t, "acme.local")

	issued := fdbPeerCertificate(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))
	if len(issued) != 1 {
		t.Fatalf("%d peer certificates issued", len(issued))
	}
	names, _, _ := unstructured.NestedSlice(issued[0].(*unstructured.Unstructured).Object, "spec", "dnsNames")

	var found bool
	for _, n := range names {
		name := n.(string)
		if strings.HasSuffix(name, ".cluster.local") {
			t.Errorf("the peer certificate names %q on a cluster whose domain is acme.local", name)
		}
		if name == "*."+ComponentFDBCluster+".simplyblock.svc.acme.local" {
			found = true
		}
	}
	if !found {
		t.Errorf("the peer certificate %v does not cover the pods under acme.local", names)
	}
}
