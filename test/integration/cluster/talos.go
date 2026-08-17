// Package cluster creates the Talos/QEMU Kubernetes cluster an integration test
// runs against.
//
// Nodes must be able to host an NVMe-oF target, not only connect to one. Talos
// ships nvmet and nvmet_tcp as modules on amd64 and arm64; they are declared in
// the machine config and loaded at boot.
//
// Talos has no shell and no SSH. Work that has to touch a node — writing nvmet
// configfs — goes through a privileged pod; see package fabric.
package cluster

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config describes the cluster to create.
type Config struct {
	// Name identifies the cluster to talosctl; must be unique on the host.
	Name string

	// Controlplanes and Workers size the cluster. Workers 0 is the default and
	// gives a single node with workloads scheduled on the control plane, which
	// is enough for everything except a same-NQN target pair: that needs two
	// kernels, so those specs must ask for a worker.
	Controlplanes int
	Workers       int

	// CIDR is the cluster network. Node addresses in it are what a target
	// advertises as its traddr.
	CIDR string

	// Talos wants 2 GiB for a control plane and 1 GiB for a worker; the
	// talosctl default is 2 GiB for both, which overpays for workers.
	ControlplaneMemoryMB int
	WorkerMemoryMB       int

	// TalosctlPath overrides the binary; empty means $PATH.
	TalosctlPath string

	// WorkDir holds the kubeconfig and talosconfig. Empty means a temp dir
	// removed with the cluster.
	WorkDir string
}

func (c *Config) applyDefaults() {
	if c.Name == "" {
		c.Name = "sb-integration"
	}
	if c.Controlplanes == 0 {
		c.Controlplanes = 1
	}
	if c.CIDR == "" {
		c.CIDR = "10.5.0.0/24"
	}
	if c.ControlplaneMemoryMB == 0 {
		c.ControlplaneMemoryMB = 2048
	}
	if c.WorkerMemoryMB == 0 {
		c.WorkerMemoryMB = 1024
	}
	if c.TalosctlPath == "" {
		c.TalosctlPath = "talosctl"
	}
}

// Cluster is a running Talos cluster.
type Cluster struct {
	cfg         Config
	workDir     string
	ownWorkDir  bool
	kubeconfig  string
	talosconfig string
	stateDir    string
}

// nvmetPatch makes a node able to host a target.
//
// Module names are the kernel's, not the package's: nvmet_tcp, not nvmet-tcp.
// nvme_tcp and nvme_fabrics are the initiator side, since these nodes also
// attach volumes.
//
// configfs is absent on purpose. Talos does not mount it and the machine config
// has no knob for it, so the pod that writes nvmet configfs mounts it — the
// requirement lives next to the code that needs it.
const nvmetPatch = `machine:
  kernel:
    modules:
      - name: nvmet
      - name: nvmet_tcp
      - name: nvme_tcp
      - name: nvme_fabrics
`

// schedulablePatch lets pods run on the control plane, which a single-node
// cluster needs since there is no worker to put them on.
const schedulablePatch = `cluster:
  allowSchedulingOnControlPlanes: true
`

// Create brings up the cluster and returns once the Kubernetes API answers.
// Minutes, mostly image download on a cold cache: call it once per suite.
func Create(ctx context.Context, cfg Config) (*Cluster, error) {
	cfg.applyDefaults()

	if _, err := exec.LookPath(cfg.TalosctlPath); err != nil {
		return nil, fmt.Errorf("talosctl not found (%s): %w", cfg.TalosctlPath, err)
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return nil, fmt.Errorf("qemu-img not found; the QEMU provisioner needs QEMU: %w", err)
	}

	c := &Cluster{cfg: cfg, workDir: cfg.WorkDir}
	if c.workDir == "" {
		dir, err := os.MkdirTemp("", "sb-integration-"+cfg.Name+"-")
		if err != nil {
			return nil, err
		}
		c.workDir, c.ownWorkDir = dir, true
	}
	c.kubeconfig = filepath.Join(c.workDir, "kubeconfig")
	c.talosconfig = filepath.Join(c.workDir, "talosconfig")
	c.stateDir = filepath.Join(c.workDir, "state")

	patches := nvmetPatch
	if cfg.Workers == 0 {
		patches += schedulablePatch
	}
	patch := filepath.Join(c.workDir, "patch.yaml")
	if err := os.WriteFile(patch, []byte(patches), 0o600); err != nil {
		return nil, err
	}

	args := []string{
		"cluster", "create", "qemu",
		"--name", cfg.Name,
		"--controlplanes", fmt.Sprint(cfg.Controlplanes),
		"--workers", fmt.Sprint(cfg.Workers),
		"--cidr", cfg.CIDR,
		"--memory-controlplanes", fmt.Sprintf("%dmb", cfg.ControlplaneMemoryMB),
		"--memory-workers", fmt.Sprintf("%dmb", cfg.WorkerMemoryMB),
		// disk-image boots from an Image Factory disk image; the default preset
		// is iso, which needs more host plumbing.
		"--presets", "disk-image",
		"--config-patch", "@" + patch,
		// State and configs stay in the work dir rather than ~/.talos and
		// ~/.kube, so a test never disturbs what the person running it uses.
		"--state", c.stateDir,
		"--talosconfig-destination", c.talosconfig,
	}

	if out, err := c.run(ctx, 20*time.Minute, args...); err != nil {
		_ = c.Destroy(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("create cluster %s: %w\n%s", cfg.Name, err, out)
	}
	// cluster create writes no kubeconfig; it is a separate command, reading the
	// endpoints from the talosconfig just written.
	if out, err := c.run(ctx, 2*time.Minute,
		"--talosconfig", c.talosconfig, "kubeconfig", c.kubeconfig, "--force"); err != nil {
		_ = c.Destroy(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("fetch kubeconfig for %s: %w\n%s", cfg.Name, err, out)
	}
	return c, nil
}

// Destroy tears the cluster down. Safe to call twice, and on a cluster that
// failed to come up, so it works from a deferred cleanup.
func (c *Cluster) Destroy(ctx context.Context) error {
	out, err := c.run(ctx, 5*time.Minute,
		"cluster", "destroy", "--name", c.cfg.Name, "--state", c.stateDir)
	if err != nil && !strings.Contains(out, "not found") {
		return fmt.Errorf("destroy cluster %s: %w\n%s", c.cfg.Name, err, out)
	}
	if c.ownWorkDir {
		return os.RemoveAll(c.workDir)
	}
	return nil
}

// Kubeconfig is this cluster's kubeconfig path.
func (c *Cluster) Kubeconfig() string { return c.kubeconfig }

// Talosconfig is this cluster's talosconfig, for talosctl commands.
func (c *Cluster) Talosconfig() string { return c.talosconfig }

// WorkDir holds the kubeconfig, talosconfig and cluster state.
func (c *Cluster) WorkDir() string { return c.workDir }

// run invokes talosctl, returning combined output because talosctl explains its
// failures there.
func (c *Cluster) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.cfg.TalosctlPath, args...) //nolint:gosec // fixed binary, structured args
	out, err := cmd.CombinedOutput()
	return string(out), err
}
