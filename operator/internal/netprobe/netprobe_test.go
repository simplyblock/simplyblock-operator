// The probe pods that run before a storage node is added, and what they report.
//
// The guest probe is attached to the declared networks the way the SPDK pod will
// be, and checks the interface, the address's uniqueness, and a peer. The host
// probe runs on the host network and pings the guest's address, which is the
// path a host process needs to a storage node on its own worker. Neither is
// privileged beyond the one capability a raw socket needs.

package netprobe

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func owner() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "storage.simplyblock.io/v1alpha2", Kind: "StorageNode", Name: "node-a", UID: types.UID("uid-1"),
	}
}

func guestOptions() GuestOptions {
	return GuestOptions{
		Namespace: "simplyblock", Node: "node-a", Worker: "worker-1", Image: "registry.example/operator:v1",
		Owner:    owner(),
		Networks: `[{"name":"storage-data","namespace":"simplyblock","interface":"data0","ips":["10.10.10.101/24"]}]`,
		Checks:   []InterfaceCheck{{Interface: "data0", Address: "10.10.10.101", Peers: []string{"10.10.10.102"}}},
	}
}

func TestTheGuestProbeCarriesTheNetworksAnnotationAndRunsOnTheWorker(t *testing.T) {
	pod, err := GuestPod(guestOptions())
	if err != nil {
		t.Fatalf("GuestPod: %v", err)
	}
	if got := pod.Annotations["k8s.v1.cni.cncf.io/networks"]; got != guestOptions().Networks {
		t.Fatalf("networks annotation = %q", got)
	}
	if pod.Spec.NodeSelector["kubernetes.io/hostname"] != "worker-1" {
		t.Fatalf("nodeSelector = %v", pod.Spec.NodeSelector)
	}
	if pod.Spec.HostNetwork {
		t.Fatal("the guest probe is on the host network, which is the thing it exists to avoid")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restartPolicy = %s", pod.Spec.RestartPolicy)
	}
}

func TestTheProbeNamesItsOwnerAndItsKind(t *testing.T) {
	pod, _ := GuestPod(guestOptions())
	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].UID != "uid-1" {
		t.Fatalf("ownerReferences = %+v", pod.OwnerReferences)
	}
	if pod.Labels[KindLabel] != KindGuest || pod.Labels[NodeLabel] != "node-a" {
		t.Fatalf("labels = %v", pod.Labels)
	}
	host, _ := HostPod(HostOptions{Namespace: "simplyblock", Node: "node-a", Worker: "worker-1",
		Image: "registry.example/operator:v1", Owner: owner(), Targets: []string{"10.10.10.101"}})
	if host.Labels[KindLabel] != KindHost || host.Name == pod.Name {
		t.Fatalf("the host probe is named %q and labeled %v", host.Name, host.Labels)
	}
}

func TestNeitherProbeIsPrivileged(t *testing.T) {
	guest, _ := GuestPod(guestOptions())
	host, _ := HostPod(HostOptions{Namespace: "simplyblock", Node: "node-a", Worker: "worker-1",
		Image: "registry.example/operator:v1", Owner: owner(), Targets: []string{"10.10.10.101"}})

	for name, pod := range map[string]*corev1.Pod{"guest": guest, "host": host} {
		c := pod.Spec.Containers[0]
		sc := c.SecurityContext
		if sc == nil || (sc.Privileged != nil && *sc.Privileged) {
			t.Fatalf("%s probe is privileged", name)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s probe allows privilege escalation", name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
			len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != "NET_RAW" {
			t.Errorf("%s probe capabilities = %+v, want drop ALL and add NET_RAW", name, sc.Capabilities)
		}
	}
	if !host.Spec.HostNetwork {
		t.Fatal("the host probe is not on the host network")
	}
}

// What reaches the script is checked, because it runs in a shell.
func TestAnAddressOrInterfaceThatIsNotWhatItClaimsIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*GuestOptions){
		"an address that is a command": func(o *GuestOptions) { o.Checks[0].Address = "10.10.10.101; reboot" },
		"an address with a prefix":     func(o *GuestOptions) { o.Checks[0].Address = "10.10.10.101/24" },
		"a peer that is a hostname":    func(o *GuestOptions) { o.Checks[0].Peers = []string{"node.example.com"} },
		"an interface with a space":    func(o *GuestOptions) { o.Checks[0].Interface = "data 0" },
		"an interface with a quote":    func(o *GuestOptions) { o.Checks[0].Interface = `data0"` },
	} {
		t.Run(name, func(t *testing.T) {
			opts := guestOptions()
			mutate(&opts)
			if _, err := GuestPod(opts); err == nil {
				t.Fatal("the probe was built from a value that is not an interface or an address")
			}
		})
	}
	if _, err := HostPod(HostOptions{Namespace: "n", Node: "x", Worker: "w", Image: "i", Owner: owner(),
		Targets: []string{"1.2.3.4 && x"}}); err == nil {
		t.Fatal("the host probe was built from a target that is not an address")
	}
}

func TestTheProbeNameFitsALabelAndDiffersByKind(t *testing.T) {
	long := strings.Repeat("n", 80)
	g, h := Name(long, KindGuest), Name(long, KindHost)
	if len(g) > 63 || len(h) > 63 {
		t.Fatalf("names are %d and %d characters", len(g), len(h))
	}
	if g == h {
		t.Fatal("the two probes share a name")
	}
	if Name(long, KindGuest) != g {
		t.Fatal("the name is not stable")
	}
	if a, b := Name(long+"a", KindGuest), Name(long+"b", KindGuest); a == b {
		t.Fatal("two long node names truncated to one probe name")
	}
}

func TestTheScriptIsValidShell(t *testing.T) {
	for name, script := range map[string]string{"guest": guestScript, "host": hostScript} {
		if err := checkShellSyntax(script); err != nil {
			t.Errorf("%s script: %v", name, err)
		}
	}
}

func TestAGuestResultIsParsed(t *testing.T) {
	message := `{"interfaces":[{"name":"data0","mtu":9000}],` +
		`"duplicates":[{"interface":"data0","address":"10.10.10.101","replies":0}],` +
		`"peers":[{"interface":"data0","peer":"10.10.10.102","ok":true}]}`
	got, err := ParseGuest(message)
	if err != nil {
		t.Fatalf("ParseGuest: %v", err)
	}
	if got.Interfaces["data0"].MTU != 9000 {
		t.Errorf("interfaces = %+v", got.Interfaces)
	}
	if got.AddressInUse("data0") {
		t.Error("an address with no replies was reported in use")
	}
	if !got.PeerReached("data0", "10.10.10.102") {
		t.Error("a peer that answered was reported unreachable")
	}
	if got.PeerReached("data0", "10.10.10.200") {
		t.Error("a peer that was never checked was reported reached")
	}
}

func TestAnUnreadableResultIsAnError(t *testing.T) {
	for _, message := range []string{"", "not json", "[]"} {
		if _, err := ParseGuest(message); err == nil {
			t.Errorf("ParseGuest(%q) did not fail", message)
		}
	}
}

func TestAHostResultIsParsed(t *testing.T) {
	got, err := ParseHost(`{"targets":[{"address":"10.10.10.101","ok":true},{"address":"10.10.10.102","ok":false}]}`)
	if err != nil {
		t.Fatalf("ParseHost: %v", err)
	}
	if !got.Reached("10.10.10.101") || got.Reached("10.10.10.102") || got.Reached("10.10.10.103") {
		t.Fatalf("result = %+v", got)
	}
	if got.AllReached() {
		t.Fatal("a result with an unreached target was reported fully reached")
	}
}

// checkShellSyntax asks the shell to parse a script without running it.
func checkShellSyntax(script string) error {
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
		return &syntaxError{out: string(out), err: err}
	}
	return nil
}

type syntaxError struct {
	out string
	err error
}

func (e *syntaxError) Error() string { return e.err.Error() + ": " + e.out }

// The host probe pings the guest's address, so the guest has to keep its network
// namespace after its checks finish. A second container that only waits does
// that, and the pod's deadline bounds it if nothing deletes the pod.
func TestTheGuestProbeKeepsItsNetworkAfterTheChecksFinish(t *testing.T) {
	pod, err := GuestPod(guestOptions())
	if err != nil {
		t.Fatalf("GuestPod: %v", err)
	}

	if len(pod.Spec.Containers) != 2 || pod.Spec.Containers[0].Name != "netprobe" || pod.Spec.Containers[1].Name != HoldContainer {
		t.Fatalf("containers = %+v, want the checks and then a holder", pod.Spec.Containers)
	}
	if hold := strings.Join(pod.Spec.Containers[1].Command, " "); !strings.Contains(hold, "sleep") {
		t.Fatalf("the holder runs %q, want it to wait", hold)
	}
}

func TestTheAddressesThatAnsweredAreReportedByInterface(t *testing.T) {
	got, err := ParseGuest(`{"duplicates":[` +
		`{"interface":"data0","address":"10.10.10.101","replies":0},` +
		`{"interface":"data0","address":"10.10.10.102","replies":2},` +
		`{"interface":"data0","address":"10.10.10.103","replies":1},` +
		`{"interface":"data1","address":"10.20.20.101","replies":1}]}`)
	if err != nil {
		t.Fatal(err)
	}

	if answering := got.AnsweringAddresses("data0"); !slices.Equal(answering, []string{"10.10.10.102", "10.10.10.103"}) {
		t.Fatalf("data0 answering = %v, want .102 and .103 in the order checked", answering)
	}
	if answering := got.AnsweringAddresses("data1"); !slices.Equal(answering, []string{"10.20.20.101"}) {
		t.Fatalf("data1 answering = %v", answering)
	}
	if !got.AddressInUse("data0") {
		t.Fatal("an address with a reply was not reported in use")
	}
	if answering := got.AnsweringAddresses("data2"); len(answering) != 0 {
		t.Fatalf("an interface never checked answered: %v", answering)
	}
}

// The candidates are checked with the same duplicate-address check as the
// address the probe holds, and are never attached.
func TestTheGuestProbeChecksTheCandidatesBeyondItsOwnAddress(t *testing.T) {
	opts := guestOptions()
	opts.Checks[0].Candidates = []string{"10.10.10.102", "10.10.10.103"}

	pod, err := GuestPod(opts)
	if err != nil {
		t.Fatalf("GuestPod: %v", err)
	}

	want := "data0=10.10.10.101,data0=10.10.10.102,data0=10.10.10.103"
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "ARPING" && e.Value != want {
			t.Fatalf("ARPING = %q, want %q", e.Value, want)
		}
	}
	if !slices.ContainsFunc(pod.Spec.Containers[0].Env, func(e corev1.EnvVar) bool { return e.Name == "ARPING" }) {
		t.Fatal("the probe has no ARPING list")
	}
}

func TestACandidateThatIsNotAnAddressIsRefused(t *testing.T) {
	opts := guestOptions()
	opts.Checks[0].Candidates = []string{"10.10.10.102; reboot"}
	if _, err := GuestPod(opts); err == nil {
		t.Fatal("a candidate that is not an address reached the shell")
	}
}

// Both probes are bounded by a deadline, so a lost operator leaves none running,
// and leave quickly when deleted, so a lingering probe does not delay the node's
// next one.
func TestBothProbesAreBoundedAndLeaveQuickly(t *testing.T) {
	guest, err := GuestPod(guestOptions())
	if err != nil {
		t.Fatal(err)
	}
	host, err := HostPod(HostOptions{
		Namespace: "simplyblock", Node: "node-a", Worker: "worker-1", Image: "registry.example/operator:v1",
		Owner: owner(), Targets: []string{"10.10.10.101"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, pod := range map[string]*corev1.Pod{"guest": guest, "host": host} {
		if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds <= 0 {
			t.Errorf("the %s probe has no deadline", name)
		}
		grace := pod.Spec.TerminationGracePeriodSeconds
		if grace == nil || *grace > 2 {
			t.Errorf("%s probe terminationGracePeriodSeconds = %v, want at most 2", name, grace)
		}
	}
}

// A shell that runs sleep as a child does not pass SIGTERM on to it, so the
// holder would wait out the grace period. exec makes sleep the container's own
// process.
func TestTheHolderEndsOnTheSignalAKubeletSends(t *testing.T) {
	pod, err := GuestPod(guestOptions())
	if err != nil {
		t.Fatal(err)
	}
	var command []string
	for _, c := range pod.Spec.Containers {
		if c.Name == HoldContainer {
			command = c.Command
		}
	}
	if len(command) == 0 || !strings.Contains(strings.Join(command, " "), "exec sleep") {
		t.Fatalf("holder command = %v, want it to exec sleep so the container's process receives SIGTERM", command)
	}
}
