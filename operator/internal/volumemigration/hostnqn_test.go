package volumemigration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/nqn"
)

func TestHostNQNOfNodeIsTheCSIsIdentity(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	uid := types.UID("10c1949d-2765-4062-8f89-14267e28a0d1")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", UID: uid}}).Build()

	got, err := HostNQNOfNode(context.Background(), cl, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := nqn.Host(string(uid)); got != want {
		t.Fatalf("host NQN = %q, want %q", got, want)
	}
	if _, err := HostNQNOfNode(context.Background(), cl, "missing"); err == nil {
		t.Fatal("a node that does not exist must be an error, not the kernel default")
	}
}

func TestEveryPathIsConnectedAsTheNodesHost(t *testing.T) {
	host := nqn.Host("10c1949d-2765-4062-8f89-14267e28a0d1")
	conns := WithHostNQN([]Connection{{NQN: "n", IP: "10.0.0.1", Port: 4420}, {NQN: "n", IP: "10.0.0.2", Port: 4420}}, host)
	for _, c := range conns {
		tgt := c.target()
		if tgt.HostNQN != host {
			t.Fatalf("target %s connects as %q, want %q", c.Address(), tgt.HostNQN, host)
		}
	}
	if got := WithHostNQN(conns[:1], ""); got[0].HostNQN != host {
		t.Fatal("an empty host NQN must leave the connections unchanged")
	}
}

func TestTheHostNQNTravelsToTheJob(t *testing.T) {
	// VMIG_CONNECTIONS is the JSON the Job reads; a Job built by an older
	// operator sends no hostNQN and connects as before.
	var conns []Connection
	if err := json.Unmarshal([]byte(`[{"nqn":"n","ip":"10.0.0.1","port":4420,"transport":"tcp","hostNQN":"h"}]`), &conns); err != nil {
		t.Fatal(err)
	}
	if len(conns) != 1 || conns[0].HostNQN != "h" {
		t.Fatalf("conns=%+v", conns)
	}
	out, err := json.Marshal([]Connection{{NQN: "n"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "hostNQN") {
		t.Fatalf("an unset host NQN must be omitted: %s", out)
	}
}
