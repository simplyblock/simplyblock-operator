package relay

// The hub's half of the site link around atlas-lib's relay: each member gets
// the ServiceAccount its agent requests a link token for and the ConfigMap that
// names the relay, and the relay's certificate outlives a restart of the
// manager, since every agent trusts the CA it was first given.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/fleet/internal/sitelink"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := clusterv1.Install(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func member(name string) *clusterv1.ManagedCluster {
	return &clusterv1.ManagedCluster{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func reconcileMember(t *testing.T, r *MemberReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

// A member gets the site link's ServiceAccount and the relay's endpoint in its
// namespace on the hub.
func TestAMemberGetsItsSiteLinkObjects(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(member("site-a")).Build()
	r := &MemberReconciler{Client: cl, Address: "192.0.2.10:30443", CA: []byte("CA-1")}
	reconcileMember(t, r, "site-a")

	var sa corev1.ServiceAccount
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "site-a", Name: sitelink.ServiceAccount}, &sa); err != nil {
		t.Errorf("the site link's ServiceAccount: %v", err)
	}
	var cm corev1.ConfigMap
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "site-a", Name: sitelink.EndpointConfigMap}, &cm); err != nil {
		t.Fatalf("the endpoint ConfigMap: %v", err)
	}
	if cm.Data[sitelink.AddressKey] != "192.0.2.10:30443" || cm.Data[sitelink.CAKey] != "CA-1" {
		t.Errorf("endpoint = %v", cm.Data)
	}
}

// A changed endpoint is rewritten on the next pass.
func TestAMembersEndpointFollowsTheRelay(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(member("site-a")).Build()
	reconcileMember(t, &MemberReconciler{Client: cl, Address: "192.0.2.10:30443", CA: []byte("CA-1")}, "site-a")
	reconcileMember(t, &MemberReconciler{Client: cl, Address: "192.0.2.11:30443", CA: []byte("CA-2")}, "site-a")

	var cm corev1.ConfigMap
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "site-a", Name: sitelink.EndpointConfigMap}, &cm); err != nil {
		t.Fatalf("the endpoint ConfigMap: %v", err)
	}
	if cm.Data[sitelink.AddressKey] != "192.0.2.11:30443" || cm.Data[sitelink.CAKey] != "CA-2" {
		t.Errorf("endpoint = %v, want the relay's new address and CA", cm.Data)
	}
}

// The relay's certificate is made once and read back after that, so a manager
// restart does not invalidate the CA every agent trusts.
func TestTheRelayCertificateSurvivesARestart(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	_, firstCA, err := LoadOrCreateTLS(context.Background(), cl, "simplyblock", []string{"192.0.2.10"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	_, secondCA, err := LoadOrCreateTLS(context.Background(), cl, "simplyblock", []string{"192.0.2.10"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if string(firstCA) != string(secondCA) {
		t.Error("the relay's CA changed across a restart")
	}
}
