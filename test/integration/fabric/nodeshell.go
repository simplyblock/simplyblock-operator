// Package fabric drives the NVMe-oF fabric inside the cluster: loading modules,
// standing up nvmet targets, and reading back what the kernel made of them.
//
// Talos has no shell and no SSH, so everything here goes through a privileged pod
// pinned to a node. That is the same access the CSI driver itself has, which makes
// it a fair way to reach a node rather than a workaround.
package fabric

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Namespace holds the harness's own pods, kept out of the namespaces a test
// creates for workloads.
const Namespace = "sb-integration"

// Shell is command execution on one node.
type Shell struct {
	cluster Cluster
	node    string
	pod     string
}

// Cluster is the part of *cluster.Cluster this package needs. An interface so
// fabric does not depend on how the cluster was created.
type Cluster interface {
	Apply(ctx context.Context, manifest string) error
	Delete(ctx context.Context, manifest string) error
	Exec(ctx context.Context, namespace, pod, script string) (string, error)
	WaitPodReady(ctx context.Context, namespace, name string, timeout time.Duration) error
}

// hostPID and the host mounts are what make this useful: modprobe has to affect
// the node's kernel, and nvmet's configfs tree is the node's, not the pod's.
//
// /lib/modules is mounted from the host because Talos keeps modules there and a
// container image has none. nsenter into PID 1's mount namespace is what lets
// modprobe and mount take effect on the node rather than inside the pod.
const shellPodManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: ` + Namespace + `
---
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: ` + Namespace + `
  labels:
    app: sb-node-shell
spec:
  nodeName: %[2]s
  hostPID: true
  hostNetwork: true
  restartPolicy: Never
  tolerations:
    - operator: Exists
  containers:
    - name: shell
      image: alpine:3.20
      command: ["sleep", "infinity"]
      securityContext:
        privileged: true
      volumeMounts:
        - name: sys
          mountPath: /sys
        - name: modules
          mountPath: /lib/modules
          readOnly: true
        - name: dev
          mountPath: /dev
  volumes:
    - name: sys
      hostPath: {path: /sys}
    - name: modules
      hostPath: {path: /lib/modules}
    - name: dev
      hostPath: {path: /dev}
`

// NewShell starts a privileged pod on node and waits for it to be ready.
func NewShell(ctx context.Context, c Cluster, node string) (*Shell, error) {
	s := &Shell{cluster: c, node: node, pod: "sb-shell-" + sanitize(node)}
	if err := c.Apply(ctx, s.manifest()); err != nil {
		return nil, fmt.Errorf("start shell on %s: %w", node, err)
	}
	if err := c.WaitPodReady(ctx, Namespace, s.pod, 3*time.Minute); err != nil {
		return nil, fmt.Errorf("shell on %s never became ready: %w", node, err)
	}
	return s, nil
}

func (s *Shell) manifest() string {
	return fmt.Sprintf(shellPodManifest, s.pod, s.node)
}

// Close removes the pod. The namespace is left, since other shells may share it.
func (s *Shell) Close(ctx context.Context) error {
	return s.cluster.Delete(ctx, s.manifest())
}

// Run executes a command inside the pod — the pod's own namespaces, which is
// enough for anything reading /sys or /dev.
func (s *Shell) Run(ctx context.Context, script string) (string, error) {
	return s.cluster.Exec(ctx, Namespace, s.pod, script)
}

// RunOnHost executes a command in the node's namespaces via PID 1, which is what
// modprobe and mount need to affect the node rather than the container.
func (s *Shell) RunOnHost(ctx context.Context, script string) (string, error) {
	return s.Run(ctx, fmt.Sprintf(
		"nsenter --target 1 --mount --uts --ipc --net --pid -- /bin/sh -c %s", shellQuote(script)))
}

// Node is the node this shell runs on.
func (s *Shell) Node() string { return s.node }

// sanitize turns a node name into something usable as a pod name.
func sanitize(node string) string {
	return strings.ToLower(strings.NewReplacer(".", "-", "_", "-", ":", "-").Replace(node))
}

// shellQuote wraps a script as a single-quoted shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
