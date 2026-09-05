// Package fabric drives the NVMe-oF fabric inside the cluster: loading modules,
// standing up nvmet targets, and reading back what the kernel made of them.
//
// Talos has no shell and no SSH, so this works through a privileged pod pinned to
// a node — the same access the CSI driver has.
//
// The pod does not enter the node's namespaces, and does not need to. Loading a
// module is not namespaced, so modprobe from a privileged container with the
// node's /lib/modules affects the node's kernel. nvmet's configfs objects are
// kernel-global for the same reason: mounting configfs inside the pod exposes the
// node's nvmet tree, not a private copy. Entering the host mount namespace would
// in fact be worse than unnecessary — there is no /bin/sh on the other side of it.
package fabric

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// Namespace holds the harness's own pods, away from workload namespaces.
	Namespace = "sb-integration"

	// ConfigFS is where the shell mounts configfs. A path of its own rather than
	// /sys/kernel/config: /sys is a bind mount from the node and mounting onto a
	// subdirectory of it invites propagation questions that this does not need to
	// answer.
	ConfigFS = "/configfs"

	// NvmetRoot is the nvmet tree once configfs is mounted.
	NvmetRoot = ConfigFS + "/nvmet"
)

// Shell is command execution on one node.
type Shell struct {
	cluster Cluster
	node    string
	pod     string
	image   string
}

// defaultShellImage is the image a Shell runs unless a test overrides it. It
// installs nothing at test time, so the default toolbox is what BusyBox and
// the kernel provide.
const defaultShellImage = "alpine:3.20"

// ShellOption adjusts how a Shell's pod is built.
type ShellOption func(*Shell)

// WithImage runs the shell pod on a different image. The reason to override is
// a test whose subject is a userspace tool BusyBox does not carry — the blkid
// probe suite needs util-linux's blkid, whose exit-code contract is the thing
// under test, and BusyBox's applet is not that binary. The image must still
// have a /bin/sh, and it must still install nothing at test time.
func WithImage(image string) ShellOption {
	return func(s *Shell) { s.image = image }
}

// Cluster is the part of *cluster.Cluster this package needs, as an interface so
// fabric does not depend on how the cluster was created.
type Cluster interface {
	Apply(ctx context.Context, manifest string) error
	Delete(ctx context.Context, manifest string) error
	Exec(ctx context.Context, namespace, pod, script string) (string, error)
	WaitPodReady(ctx context.Context, namespace, name string, timeout time.Duration) error
}

// The namespace carries pod-security labels because Talos enforces the baseline
// profile by default, which allows none of what this pod needs: host namespaces,
// hostPath volumes, or a privileged container.
//
// The container installs nothing, so this package uses only what BusyBox and the
// kernel provide. Anything else would make a test depend on a package mirror.
const shellPodManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: ` + Namespace + `
  labels:
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/warn: privileged
    pod-security.kubernetes.io/audit: privileged
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
      image: %[3]s
      command: ["/bin/sh", "-c", "exec sleep infinity"]
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
func NewShell(ctx context.Context, c Cluster, node string, opts ...ShellOption) (*Shell, error) {
	s := &Shell{cluster: c, node: node, pod: "sb-shell-" + sanitize(node), image: defaultShellImage}
	for _, opt := range opts {
		opt(s)
	}
	if s.image != defaultShellImage {
		// Two shells on one node must not fight over one pod name when they run
		// different images: the second Apply would try to mutate the immutable
		// image field of the first one's pod.
		s.pod = "sb-shell-" + sanitize(s.image) + "-" + sanitize(node)
	}
	if err := c.Apply(ctx, s.manifest()); err != nil {
		return nil, fmt.Errorf("start shell on %s: %w", node, err)
	}
	if err := c.WaitPodReady(ctx, Namespace, s.pod, 3*time.Minute); err != nil {
		return nil, fmt.Errorf("shell on %s never became ready: %w", node, err)
	}
	return s, nil
}

func (s *Shell) manifest() string {
	return fmt.Sprintf(shellPodManifest, s.pod, s.node, s.image)
}

// Close removes the pod, leaving the namespace since other shells may share it.
func (s *Shell) Close(ctx context.Context) error {
	return s.cluster.Delete(ctx, s.manifest())
}

// Run executes a command in the pod.
func (s *Shell) Run(ctx context.Context, script string) (string, error) {
	return s.cluster.Exec(ctx, Namespace, s.pod, script)
}

// EnsureConfigFS mounts configfs and returns the nvmet root. Talos does not mount
// configfs, and nvmet cannot be configured without it. Idempotent.
func (s *Shell) EnsureConfigFS(ctx context.Context) (string, error) {
	out, err := s.Run(ctx, fmt.Sprintf(
		"mkdir -p %[1]s && (mountpoint -q %[1]s || mount -t configfs none %[1]s) && ls -d %[2]s",
		ConfigFS, NvmetRoot))
	if err != nil {
		return "", fmt.Errorf("mount configfs on %s: %w\n%s", s.node, err, out)
	}
	if !strings.Contains(out, NvmetRoot) {
		return "", fmt.Errorf("nvmet absent under %s on %s; is the nvmet module loaded?\n%s",
			ConfigFS, s.node, out)
	}
	return NvmetRoot, nil
}

// LoadedModules is the node's loaded kernel modules. Module state is not
// namespaced, so the pod's /proc/modules is the node's.
func (s *Shell) LoadedModules(ctx context.Context) (string, error) {
	return s.Run(ctx, "cat /proc/modules")
}

// Node is the node this shell runs on.
func (s *Shell) Node() string { return s.node }

// sanitize turns a node name — or an image reference — into something usable
// as a pod name.
func sanitize(node string) string {
	return strings.ToLower(strings.NewReplacer(".", "-", "_", "-", ":", "-", "/", "-").Replace(node))
}
