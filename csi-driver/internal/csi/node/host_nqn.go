// The host NQN this node's kernel presents on a plain volume connect, published
// on the Node for the operator.
//
// A volume connected without --hostnqn uses /etc/nvme/hostnqn, which the
// plugin's postStart hook writes from the hostid persisted in /var/lib/nvme. On
// a two-node cluster the control plane revokes every host identity of a fenced
// node on its peer, and only the plugin knows this one: the operator derives the
// other, nqn.Host, from the node's UID. It lives beside the subnet and VDO
// probes, which publish the node's other facts the same way.

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/nqn"
)

// AdvertiseHostNQN sets nodeName's kube.AnnoNVMeHostNQN to the kernel's default
// host NQN, as nqn.DefaultHost reads it. Runs once at process start, like the
// subnet probe.
func AdvertiseHostNQN(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) error {
	return advertiseHostNQN(ctx, kubeClient, nodeName, os.ReadFile)
}

func advertiseHostNQN(
	ctx context.Context, kubeClient kubernetes.Interface, nodeName string,
	read func(string) ([]byte, error),
) error {
	want, err := nqn.DefaultHost(read)
	if err != nil {
		return err
	}
	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}
	if node.Annotations[kube.AnnoNVMeHostNQN] == want {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{kube.AnnoNVMeHostNQN: want}}})
	if err != nil {
		return fmt.Errorf("build the host NQN patch: %w", err)
	}
	if _, err := kubeClient.CoreV1().Nodes().Patch(
		ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("annotate node %s with its host NQN: %w", nodeName, err)
	}
	klog.Infof("host NQN: node %s annotated %s", nodeName, want)
	return nil
}
