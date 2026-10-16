// Unit tests for the exports' stable mount address: the metadata server's one
// Service and EndpointSlice, which binding stands up and every export of that
// metadata server shares.

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// mdsServiceKey is the bound metadata server's Service.
func mdsServiceKey() client.ObjectKey {
	return client.ObjectKey{
		Namespace: testOperatorNS,
		Name:      utils.MDSServiceName(driver.MDSPodName(mdsDriver(), testExportClusterID)),
	}
}

// sharedServiceIP is the metadata server's Service address in the sharing test.
const sharedServiceIP = "10.96.7.7"

// mdsSliceKeyFor is the EndpointSlice of the metadata server whose pod is pod.
func mdsSliceKeyFor(pod string) client.ObjectKey {
	return client.ObjectKey{Namespace: testOperatorNS, Name: utils.MDSEndpointSliceName(utils.MDSServiceName(pod))}
}

// Binding an export points it at the metadata server's Service, in the
// operator's namespace, with an EndpointSlice naming the metadata server pod,
// and records the Service's address as ServiceAddress, separately from
// MDSNodeIP, the pod's own address. No Service of the export's own is made.
func TestBindingUsesTheMetadataServersService(t *testing.T) {
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), mdsDriver(), boundMDSPod(readyPod))

	reconcileExport(t, r)

	var svc corev1.Service
	if err := cl.Get(context.Background(), mdsServiceKey(), &svc); err != nil {
		t.Fatalf("reading the metadata server's Service: %v", err)
	}
	for _, ref := range svc.OwnerReferences {
		if ref.Kind == "NFSExport" {
			t.Errorf("the Service is owned by export %s, so deleting the export deletes every export's address", ref.Name)
		}
	}

	var eps discoveryv1.EndpointSlice
	epsKey := client.ObjectKey{Namespace: testOperatorNS, Name: utils.MDSEndpointSliceName(svc.Name)}
	if err := cl.Get(context.Background(), epsKey, &eps); err != nil {
		t.Fatalf("reading the EndpointSlice: %v", err)
	}
	if len(eps.Endpoints) != 1 || len(eps.Endpoints[0].Addresses) != 1 {
		t.Fatalf("endpoints = %+v, want exactly one address", eps.Endpoints)
	}
	got := loadExport(t, cl)
	if want := got.Status.MDSNodeIP; eps.Endpoints[0].Addresses[0] != want {
		t.Errorf("EndpointSlice address = %q, want the bound host's address %q", eps.Endpoints[0].Addresses[0], want)
	}
	if eps.Labels["kubernetes.io/service-name"] != svc.Name {
		t.Errorf("EndpointSlice does not label itself for %s: labels = %v", svc.Name, eps.Labels)
	}
	if got.Status.ServiceAddress != svc.Spec.ClusterIP {
		t.Errorf("status.serviceAddress = %q, want the Service's ClusterIP %q", got.Status.ServiceAddress, svc.Spec.ClusterIP)
	}

	var own corev1.Service
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: testExportNS, Name: testExportName + "-nfs"}, &own)
	if !apierrors.IsNotFound(err) {
		t.Errorf("a Service of the export's own exists (%v)", err)
	}
}

// Every export of one metadata server is mounted at one address. The Linux
// client keeps one NFS client per server and trunks a mount at another address
// of the same server onto it, so with an address per export, every mount on a
// node rode on the first export's address and stalled once that export was
// deleted (dev-ocp pnfs-1792093629: a 48s freeze and 115-226s write stalls).
func TestExportsOfOneMetadataServerShareOneAddress(t *testing.T) {
	existing := utils.BuildMDSService(testOperatorNS, mdsServiceKey().Name)
	existing.Spec.ClusterIP = sharedServiceIP
	existing.ResourceVersion = "1"
	second := testExport(func(e *simplyblockv1alpha2.NFSExport) { e.Name = testExportName + "-2" })

	r, cl, _ := newPodHostedReconciler(t, testExport(nil), mdsDriver(), boundMDSPod(readyPod), existing, second)

	reconcileExport(t, r)
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(second)}); err != nil {
		t.Fatalf("Reconcile second: %v", err)
	}

	var e1, e2 simplyblockv1alpha2.NFSExport
	_ = cl.Get(context.Background(), client.ObjectKey{Namespace: testExportNS, Name: testExportName}, &e1)
	_ = cl.Get(context.Background(), client.ObjectKeyFromObject(second), &e2)
	if e1.Status.ServiceAddress != sharedServiceIP || e2.Status.ServiceAddress != sharedServiceIP {
		t.Errorf("serviceAddress = %q and %q, want both at the metadata server's %s",
			e1.Status.ServiceAddress, e2.Status.ServiceAddress, sharedServiceIP)
	}
	var services corev1.ServiceList
	if err := cl.List(context.Background(), &services); err != nil {
		t.Fatalf("listing Services: %v", err)
	}
	var nfs []string
	for _, s := range services.Items {
		if strings.HasSuffix(s.Name, "-nfs") {
			nfs = append(nfs, s.Namespace+"/"+s.Name)
		}
	}
	if len(nfs) != 1 {
		t.Errorf("NFS Services = %v, want the metadata server's one", nfs)
	}
}

// A ClusterIP is allocated once and must never be reassigned out from under an
// already-mounted client, so a Service that already exists is left alone
// rather than recreated.
func TestBindingKeepsAnAlreadyAllocatedClusterIP(t *testing.T) {
	existing := utils.BuildMDSService(testOperatorNS, mdsServiceKey().Name)
	existing.Spec.ClusterIP = "10.96.5.5"
	existing.ResourceVersion = "1"

	r, cl, _ := newPodHostedReconciler(t, testExport(nil), mdsDriver(), boundMDSPod(readyPod), existing)

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.ServiceAddress != "10.96.5.5" {
		t.Errorf("status.serviceAddress = %q, want the already-allocated ClusterIP", got.Status.ServiceAddress)
	}
}

// The metadata server's Service is owned by its StatefulSet, without
// blockOwnerDeletion: setting it needs the right to set finalizers on the
// owner, which OpenShift enforces and the operator does not hold for
// StatefulSets, so creating the Service was forbidden and every export stayed
// unbound (dev-ocp, pnfs-1792138910). The pod's own reference to the
// StatefulSet carries blockOwnerDeletion, so a copy of it must drop it.
func TestTheMetadataServersServiceDoesNotBlockItsOwnersDeletion(t *testing.T) {
	pod := boundMDSPod(func(p *corev1.Pod) {
		readyPod(p)
		p.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "StatefulSet", Name: "simplyblock-pnfs-mds-x",
			UID: "sts-uid", Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
		}}
	})
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), mdsDriver(), pod)

	reconcileExport(t, r)

	var svc corev1.Service
	if err := cl.Get(context.Background(), mdsServiceKey(), &svc); err != nil {
		t.Fatalf("reading the metadata server's Service: %v", err)
	}
	if len(svc.OwnerReferences) != 1 || svc.OwnerReferences[0].Kind != "StatefulSet" {
		t.Fatalf("owner references = %+v, want the StatefulSet", svc.OwnerReferences)
	}
	if b := svc.OwnerReferences[0].BlockOwnerDeletion; b != nil && *b {
		t.Error("the Service's owner reference sets blockOwnerDeletion")
	}
	var eps discoveryv1.EndpointSlice
	if err := cl.Get(context.Background(), mdsSliceKeyFor(pod.Name), &eps); err != nil {
		t.Fatalf("reading the EndpointSlice: %v", err)
	}
	for _, ref := range eps.OwnerReferences {
		if ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion {
			t.Error("the EndpointSlice's owner reference sets blockOwnerDeletion")
		}
	}
}
