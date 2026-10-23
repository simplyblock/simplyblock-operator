// What the node plugin publishes as its kernel host NQN, which the control plane
// revokes on the peer when it fences this node's host on a two-node cluster.

package node

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/nqn"
)

func files(m map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if v, ok := m[path]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("no such file")
	}
}

func nodeAnnotation(t *testing.T, client *kfake.Clientset) string {
	t.Helper()
	node, err := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return node.Annotations[kube.AnnoNVMeHostNQN]
}

func TestAdvertiseHostNQNPublishesTheKernelDefault(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(nil, nil))
	read := files(map[string]string{nqn.DefaultHostNQNFile: "nqn.2014-08.org.nvmexpress:uuid:1111\n"})
	if err := advertiseHostNQN(context.Background(), client, "node-a", read); err != nil {
		t.Fatalf("advertiseHostNQN: %v", err)
	}
	if got := nodeAnnotation(t, client); got != "nqn.2014-08.org.nvmexpress:uuid:1111" {
		t.Errorf("annotation = %q", got)
	}
}

// Without either file there is nothing true to publish: an invented NQN would
// make the control plane revoke a host identity nobody uses.
func TestAdvertiseHostNQNPublishesNothingWithoutAHostID(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(nil, nil))
	if err := advertiseHostNQN(context.Background(), client, "node-a", files(nil)); err == nil {
		t.Fatal("expected an error without a host id")
	}
	if got := nodeAnnotation(t, client); got != "" {
		t.Errorf("annotation = %q, want none", got)
	}
}

func TestAdvertiseHostNQNKeepsOtherAnnotations(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(nil, map[string]string{"other": "x"}))
	read := files(map[string]string{nqn.DefaultHostNQNFile: "nqn.2014-08.org.nvmexpress:uuid:3333"})
	if err := advertiseHostNQN(context.Background(), client, "node-a", read); err != nil {
		t.Fatalf("advertiseHostNQN: %v", err)
	}
	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Annotations["other"] != "x" {
		t.Errorf("other annotation lost: %v", node.Annotations)
	}
}
