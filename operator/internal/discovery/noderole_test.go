// What a node's role is read as, and why the answer is not its taints.
//
// The case that motivates the file is the OpenShift infrastructure node: it is
// labeled and usually not tainted, so every check the operator had before this
// passed it straight through and it became a storage worker.

package discovery

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func labeled(labels map[string]string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Labels: labels}}
}

func TestTheRoleIsReadOffTheLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   NodeRole
	}{
		{
			name:   "no label at all is a worker",
			labels: nil,
			want:   NodeRoleWorker,
		},
		{
			name:   "an explicit worker label",
			labels: map[string]string{"node-role.kubernetes.io/worker": ""},
			want:   NodeRoleWorker,
		},
		{
			name:   "a control-plane node",
			labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
			want:   NodeRoleControlPlane,
		},
		{
			// The older spelling, still written by clusters installed before it
			// changed. Reading only the new one would put storage nodes on the
			// control plane of every such fleet.
			name:   "the older master spelling",
			labels: map[string]string{"node-role.kubernetes.io/master": ""},
			want:   NodeRoleControlPlane,
		},
		{
			name:   "an OpenShift infrastructure node",
			labels: map[string]string{"node-role.kubernetes.io/infra": ""},
			want:   NodeRoleInfra,
		},
		{
			// A combined deployment marks one machine both ways. The most
			// restrictive wins: a machine that runs etcd runs etcd whatever else
			// it also does.
			name: "both control-plane and worker",
			labels: map[string]string{
				"node-role.kubernetes.io/control-plane": "",
				"node-role.kubernetes.io/worker":        "",
			},
			want: NodeRoleControlPlane,
		},
		{
			// A fleet labels machines for its own purposes, and a storage node
			// belongs on one of those unless somebody says otherwise.
			name:   "a role this product does not know",
			labels: map[string]string{"node-role.kubernetes.io/gpu": ""},
			want:   NodeRoleWorker,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoleOf(labeled(tc.labels)); got != tc.want {
				t.Errorf("RoleOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// The whole reason this is a label check rather than a taint check: an
// infrastructure node carries no taint on most OpenShift fleets, so every check
// the operator had before this one saw an ordinary worker.
func TestAnUntaintedInfraNodeIsRecognized(t *testing.T) {
	node := labeled(map[string]string{"node-role.kubernetes.io/infra": ""})
	if len(node.Spec.Taints) != 0 {
		t.Fatal("the fixture is tainted, which is not the case this is about")
	}

	role := RoleOf(node)
	if role != NodeRoleInfra {
		t.Fatalf("RoleOf = %q, want Infra", role)
	}
	// It is the tier storage belongs on rather than one to avoid, so recognizing
	// it is what lets a draft propose the placement the fleet intended.
	if !role.HoldsStorageNodes() {
		t.Error("an infrastructure node was refused, and it is where storage belongs")
	}
}

// Infra and Worker both hold storage nodes. ControlPlane does not, which is the
// conservative half of a decision with a real cost: a fleet that wants storage on
// its etcd hosts has to say so.
func TestOnlyTheControlPlaneIsHeldBack(t *testing.T) {
	for role, want := range map[NodeRole]bool{
		NodeRoleWorker:       true,
		NodeRoleInfra:        true,
		NodeRoleControlPlane: false,
	} {
		if got := role.HoldsStorageNodes(); got != want {
			t.Errorf("%s.HoldsStorageNodes() = %v, want %v", role, got, want)
		}
	}
}

// The infrastructure tier is proposed ahead of the workers, because a fleet with
// disks in both almost always meant the infra nodes to be the storage.
func TestTheInfrastructureTierIsPreferred(t *testing.T) {
	if NodeRoleInfra.Preference() >= NodeRoleWorker.Preference() {
		t.Error("a worker is proposed ahead of an infrastructure node")
	}
	if NodeRoleWorker.Preference() >= NodeRoleControlPlane.Preference() {
		t.Error("a control-plane node is proposed ahead of a worker")
	}
}

// A draft reports what a machine is rather than only what it was reduced to, so
// every role its labels name survives.
func TestEveryRoleIsReported(t *testing.T) {
	node := labeled(map[string]string{
		"node-role.kubernetes.io/control-plane": "",
		"node-role.kubernetes.io/worker":        "",
	})

	roles := RolesOf(node)
	if len(roles) != 2 {
		t.Fatalf("RolesOf = %v, want both", roles)
	}
	seen := map[NodeRole]bool{}
	for _, role := range roles {
		seen[role] = true
	}
	if !seen[NodeRoleControlPlane] || !seen[NodeRoleWorker] {
		t.Errorf("RolesOf = %v, want the control-plane and worker roles", roles)
	}
}

// The role lands on the KubeNode the planner reads, so a draft can report it.
func TestTheRoleReachesTheKubeNode(t *testing.T) {
	node := labeled(map[string]string{"node-role.kubernetes.io/infra": ""})

	if got := KubeNodeOf(node).Role; got != NodeRoleInfra {
		t.Errorf("KubeNodeOf(...).Role = %q, want Infra", got)
	}
}
