// Unit tests for the export's stable mount address: the Service and
// EndpointSlice reconcilePending stands up alongside the binding.

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// Binding an export creates a Service and a matching EndpointSlice pointing
// at the bound host, and records the Service's address as ServiceAddress --
// separately from MDSNodeIP, which is the host's own address.
func TestBindingCreatesAStableAddress(t *testing.T) {
	asm := &fakeAssembler{}
	r, cl := newExportReconciler(t, asm, testExport(nil), testNode(testMDSHost, nil))

	reconcileExport(t, r)

	var svc corev1.Service
	svcKey := client.ObjectKey{Name: utils.NFSExportServiceName(testExportName), Namespace: testExportNS}
	if err := cl.Get(context.Background(), svcKey, &svc); err != nil {
		t.Fatalf("reading the Service: %v", err)
	}
	if len(svc.OwnerReferences) != 1 || svc.OwnerReferences[0].Name != testExportName {
		t.Errorf("Service owner references = %v, want one naming the export", svc.OwnerReferences)
	}

	var eps discoveryv1.EndpointSlice
	epsKey := client.ObjectKey{Name: utils.NFSExportEndpointSliceName(testExportName), Namespace: testExportNS}
	if err := cl.Get(context.Background(), epsKey, &eps); err != nil {
		t.Fatalf("reading the EndpointSlice: %v", err)
	}
	if len(eps.Endpoints) != 1 || len(eps.Endpoints[0].Addresses) != 1 {
		t.Fatalf("endpoints = %+v, want exactly one address", eps.Endpoints)
	}
	got := loadExport(t, cl)
	if want := got.Status.MDSNodeIP; eps.Endpoints[0].Addresses[0] != want {
		t.Errorf("EndpointSlice address = %q, want the bound host's address %q",
			eps.Endpoints[0].Addresses[0], want)
	}
	if eps.Labels["kubernetes.io/service-name"] != svc.Name {
		t.Errorf("EndpointSlice does not label itself for %s: labels = %v", svc.Name, eps.Labels)
	}
	if got.Status.ServiceAddress != svc.Spec.ClusterIP {
		t.Errorf("status.serviceAddress = %q, want the Service's ClusterIP %q",
			got.Status.ServiceAddress, svc.Spec.ClusterIP)
	}
}

// A ClusterIP is allocated once and must never be reassigned out from under an
// already-mounted client, so a Service that already exists is left alone
// rather than recreated.
func TestBindingKeepsAnAlreadyAllocatedClusterIP(t *testing.T) {
	existing := utils.BuildNFSExportService(testExportNS, testExportName)
	existing.Spec.ClusterIP = "10.96.5.5"
	existing.ResourceVersion = "1"

	asm := &fakeAssembler{}
	r, cl := newExportReconciler(t, asm, testExport(nil), testNode(testMDSHost, nil), existing)

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.ServiceAddress != "10.96.5.5" {
		t.Errorf("status.serviceAddress = %q, want the already-allocated ClusterIP", got.Status.ServiceAddress)
	}
}
