// How the SPDK pod's network-status annotation is read and compared with the
// declaration. The annotation is Multus's record of the attach, written once
// when the attach finishes, so it is the evidence of what the pod holds and
// says nothing about the interface afterward.

package node

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const defaultNetworkStatus = `{"name":"cbr0","interface":"eth0","ips":["10.42.1.164"],"mac":"22:38:83:cc:22:a1","default":true}`

func statusOf(entries ...string) string {
	return "[" + strings.Join(entries, ",") + "]"
}

func dataEntry(name, iface, ip string) string {
	return `{"name":"` + name + `","interface":"` + iface + `","ips":["` + ip + `"],"mac":"16:0d:ac:ae:ae:6f"}`
}

func declared(network, definition, iface, cidr string) Attachment {
	return Attachment{Network: network, Name: definition, Namespace: attachNamespace, Interface: iface, IPs: []string{cidr}}
}

func verify(t *testing.T, annotation string, running bool, want ...Attachment) []NetworkVerdict {
	t.Helper()
	got, err := VerifyAttachments(want, annotation, running)
	if err != nil {
		t.Fatalf("VerifyAttachments: %v", err)
	}
	return got
}

// U-13
func TestEveryDeclaredInterfaceHoldingItsAddressIsAttached(t *testing.T) {
	got := verify(t, statusOf(defaultNetworkStatus,
		dataEntry("simplyblock/storage-a-def", "data0", "10.10.10.101"),
		dataEntry("simplyblock/storage-b-def", "data1", "10.20.20.101")), true,
		declared("storage-a", "storage-a-def", "data0", "10.10.10.101/24"),
		declared("storage-b", "storage-b-def", "data1", "10.20.20.101/24"))

	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	for _, v := range got {
		if v.Phase != simplyblockv1alpha2.NodeNetworkPhaseAttached || v.Reported == "" {
			t.Errorf("verdict %+v, want Attached", v)
		}
	}
	if got[0].Network != "storage-a" || got[0].Interface != "data0" || got[0].Reported != "10.10.10.101" {
		t.Errorf("first verdict = %+v", got[0])
	}
}

// U-14
func TestAnAddressThatDiffersIsMismatchedAndNamesBoth(t *testing.T) {
	got := verify(t, statusOf(dataEntry("simplyblock/storage-a-def", "data0", "10.10.10.77")), true,
		declared("storage-a", "storage-a-def", "data0", "10.10.10.101/24"))

	if got[0].Phase != simplyblockv1alpha2.NodeNetworkPhaseMismatched {
		t.Fatalf("verdict %+v, want Mismatched", got[0])
	}
	if got[0].Reported != "10.10.10.77" || got[0].Declared != "10.10.10.101/24" {
		t.Fatalf("verdict %+v does not carry both addresses", got[0])
	}
}

// U-15
func TestAMissingInterfaceIsPendingBeforeTheStartAndUnavailableAfter(t *testing.T) {
	annotation := statusOf(defaultNetworkStatus)
	want := declared("storage-a", "storage-a-def", "data0", "10.10.10.101/24")

	if got := verify(t, annotation, false, want); got[0].Phase != simplyblockv1alpha2.NodeNetworkPhasePending {
		t.Errorf("before the pod runs: %+v, want Pending", got[0])
	}
	if got := verify(t, annotation, true, want); got[0].Phase != simplyblockv1alpha2.NodeNetworkPhaseUnavailable {
		t.Errorf("after the pod runs: %+v, want Unavailable", got[0])
	}
}

// U-16
func TestAnUnreadableAnnotationIsReportedAndNeverPanics(t *testing.T) {
	want := declared("storage-a", "storage-a-def", "data0", "10.10.10.101/24")
	for name, annotation := range map[string]string{"not JSON": "{not json", "wrong shape": `{"name":"x"}`} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyAttachments([]Attachment{want}, annotation, true); err == nil {
				t.Fatal("an unreadable annotation was not reported")
			}
		})
	}
	for name, annotation := range map[string]string{"missing": "", "empty list": "[]"} {
		t.Run(name, func(t *testing.T) {
			got, err := VerifyAttachments([]Attachment{want}, annotation, true)
			if err != nil {
				t.Fatalf("VerifyAttachments: %v", err)
			}
			if got[0].Phase != simplyblockv1alpha2.NodeNetworkPhaseUnavailable {
				t.Errorf("verdict %+v, want Unavailable", got[0])
			}
		})
	}
}

// U-17
func TestADefinitionIsMatchedByNamespaceAndNameNotByPrefix(t *testing.T) {
	annotation := statusOf(
		dataEntry("other/storage-a-def", "data0", "10.10.10.101"),
		dataEntry("simplyblock/storage-a-def-old", "data0", "10.10.10.101"))
	got := verify(t, annotation, true, declared("storage-a", "storage-a-def", "data0", "10.10.10.101/24"))

	if got[0].Phase == simplyblockv1alpha2.NodeNetworkPhaseAttached {
		t.Fatalf("a definition of another namespace or name satisfied the declaration: %+v", got[0])
	}
}

// U-18
func TestAddressesCompareByTheirHostPart(t *testing.T) {
	for _, tc := range []struct{ declared, reported string }{
		{"10.10.10.101/24", "10.10.10.101"},
		{"10.10.10.101/16", "10.10.10.101/24"},
		{"10.10.10.101/32", "10.10.10.101"},
	} {
		t.Run(tc.declared+" and "+tc.reported, func(t *testing.T) {
			got := verify(t, statusOf(dataEntry("simplyblock/storage-a-def", "data0", tc.reported)), true,
				declared("storage-a", "storage-a-def", "data0", tc.declared))
			if got[0].Phase != simplyblockv1alpha2.NodeNetworkPhaseAttached {
				t.Fatalf("verdict %+v, want Attached", got[0])
			}
		})
	}
}

// U-19
func TestTheDefaultNetworkAndExtraInterfacesDoNotChangeTheMatch(t *testing.T) {
	got := verify(t, statusOf(defaultNetworkStatus,
		dataEntry("simplyblock/storage-a-def", "data0", "10.10.10.101"),
		dataEntry("simplyblock/undeclared", "net9", "192.0.2.9")), true,
		declared("storage-a", "storage-a-def", "data0", "10.10.10.101/24"))

	if len(got) != 1 || got[0].Phase != simplyblockv1alpha2.NodeNetworkPhaseAttached {
		t.Fatalf("got %+v", got)
	}
}

func TestNothingDeclaredIsNothingVerified(t *testing.T) {
	if got := verify(t, statusOf(defaultNetworkStatus), true); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// U-20
func TestThePodIsFoundByItsRPCPortAndTwoPodsAreNeverConfused(t *testing.T) {
	pod := func(name, port, worker string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Labels: map[string]string{"app": "spdk-app-" + port, "role": "simplyblock-storage-node"},
			},
			Spec: corev1.PodSpec{NodeName: worker},
		}
	}
	pods := []corev1.Pod{
		pod("snode-spdk-pod-8080-abc", "8080", "worker-1"),
		pod("snode-spdk-pod-8081-abc", "8081", "worker-1"),
	}

	got, found := SPDKPodFor(pods, "worker-1", 8081)
	if !found || got.Name != "snode-spdk-pod-8081-abc" {
		t.Fatalf("got %v, %v", got, found)
	}
	if _, found := SPDKPodFor(pods, "worker-1", 808); found {
		t.Fatal("a port that is a prefix of another matched it")
	}
	if _, found := SPDKPodFor(pods, "worker-1", 9999); found {
		t.Fatal("a port no pod holds matched a pod")
	}
	if _, found := SPDKPodFor(nil, "worker-1", 8080); found {
		t.Fatal("no pods matched a pod")
	}
}

// Every worker's first SPDK pod carries the same port. The pod of another worker
// on that port is another node's, and reading it judged the node's address
// mismatched against a neighbour's (Talos lab, 2026-10-25).
func TestThePodOfAnotherWorkerOnTheSamePortIsNotTheNodes(t *testing.T) {
	pod := func(name, worker string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Labels: map[string]string{"app": "spdk-app-4420", "role": "simplyblock-storage-node"},
			},
			Spec: corev1.PodSpec{NodeName: worker},
		}
	}
	pods := []corev1.Pod{pod("snode-spdk-pod-4420-w1", "worker-1"), pod("snode-spdk-pod-4420-w2", "worker-2")}

	got, found := SPDKPodFor(pods, "worker-2", 4420)
	if !found || got.Name != "snode-spdk-pod-4420-w2" {
		t.Fatalf("got %v, %v, want worker-2's pod and not the first on the port", got, found)
	}
	if _, found := SPDKPodFor(pods, "worker-3", 4420); found {
		t.Fatal("a worker with no pod matched another worker's pod")
	}
	if got, found := SPDKPodFor(pods, "", 4420); !found || got.Name != "snode-spdk-pod-4420-w1" {
		t.Fatalf("an empty worker should match any worker's pod: got %v, %v", got, found)
	}
}
