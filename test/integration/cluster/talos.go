// Package cluster creates the Talos/QEMU Kubernetes cluster an integration test
// runs against.
//
// Nodes must be able to host an NVMe-oF target, not only connect to one. Talos
// ships nvmet and nvmet_tcp as modules on amd64 and arm64; they are declared in
// the machine config and loaded at boot.
//
// Talos has no shell and no SSH. Work that has to touch a node — writing nvmet
// configfs — goes through a privileged pod; see package fabric.
//
// talosctl's QEMU provisioner needs root, for the CNI bridge it builds and for
// the accelerator. Only talosctl is elevated, not the test process: a test
// running as root would write its build cache and artifacts as root and leave
// them for whatever runs next.
package cluster

import (
	"context"
	"encoding/json"
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

	// Sudo elevates talosctl. Nil auto-detects: false when already root, true
	// otherwise. Passwordless sudo is required either way — CI has it, and a
	// developer will be prompted.
	Sudo *bool

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
	if c.Sudo == nil {
		needed := os.Geteuid() != 0
		c.Sudo = &needed
	}
}

// Cluster is a running Talos cluster.
type Cluster struct {
	cfg         Config
	workDir     string
	ownWorkDir  bool
	kubeconfig  string
	talosconfig string
	addresses   []string
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
	if *cfg.Sudo {
		if _, err := exec.LookPath("sudo"); err != nil {
			return nil, fmt.Errorf("sudo not found, and the QEMU provisioner needs root: %w", err)
		}
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

	// An interrupted run leaves the cluster, its network and its root-owned
	// helper processes behind. Creating over the top of that fails confusingly —
	// the network exists but belongs to nobody — so clear it first. Destroying a
	// cluster that is not there is not an error.
	if out, err := c.run(ctx, 5*time.Minute, "cluster", "destroy", "--name", cfg.Name); err != nil &&
		!strings.Contains(out, "not found") {
		return nil, fmt.Errorf("clear a previous %s: %w\n%s", cfg.Name, err, out)
	}

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
		// The talosconfig and kubeconfig are redirected because they disturb the
		// developer's own; cluster state deliberately is not. It stays where
		// talosctl looks for it by default, so `talosctl cluster destroy --name`
		// can find and clean a cluster left behind by an interrupted run. State
		// in a per-run temp directory makes that impossible, which turns a
		// killed test into root-owned processes nobody can reach.
		"--talosconfig-destination", c.talosconfig,
	}

	if out, err := c.run(ctx, 20*time.Minute, args...); err != nil {
		_ = c.Destroy(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("create cluster %s: %w\n%s", cfg.Name, err, out)
	}
	// cluster create writes no kubeconfig; it is a separate command, and it needs
	// to be told which node to ask — cluster create leaves endpoints in the
	// talosconfig but no nodes, so the address has to be read back out.
	nodes, err := c.controlplaneAddresses(ctx)
	if err != nil {
		_ = c.Destroy(context.WithoutCancel(ctx))
		return nil, err
	}
	c.addresses = nodes

	if out, err := c.run(ctx, 2*time.Minute, "--talosconfig", c.talosconfig,
		"kubeconfig", c.kubeconfig, "--nodes", nodes[0], "--force"); err != nil {
		_ = c.Destroy(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("fetch kubeconfig for %s: %w\n%s", cfg.Name, err, out)
	}

	// talosctl ran as root, so everything it wrote is root-owned; the test and
	// kubectl run unprivileged and have to be able to read it.
	if err := c.reclaimWorkDir(ctx); err != nil {
		_ = c.Destroy(context.WithoutCancel(ctx))
		return nil, err
	}
	return c, nil
}

// Destroy tears the cluster down. Safe to call twice, and on a cluster that
// failed to come up, so it works from a deferred cleanup.
func (c *Cluster) Destroy(ctx context.Context) error {
	out, err := c.run(ctx, 5*time.Minute, "cluster", "destroy", "--name", c.cfg.Name)
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

// controlplaneAddresses returns the Talos API addresses of the control plane,
// which is what talosctl commands need as --nodes.
//
// The shape of `config info` output is read defensively: both fields are tried
// and an unrecognised shape reports the raw output, so a format change says what
// it changed to instead of failing as an empty list.
func (c *Cluster) controlplaneAddresses(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, time.Minute,
		"--talosconfig", c.talosconfig, "config", "info", "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("read talos context: %w\n%s", err, out)
	}

	var info struct {
		Nodes     []string `json:"nodes"`
		Endpoints []string `json:"endpoints"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return nil, fmt.Errorf("parse talos context: %w\nraw output:\n%s", err, out)
	}
	if len(info.Nodes) > 0 {
		return info.Nodes, nil
	}
	if len(info.Endpoints) > 0 {
		return info.Endpoints, nil
	}
	return nil, fmt.Errorf("talos context names neither nodes nor endpoints\nraw output:\n%s", out)
}

// Addresses are the cluster's Talos API addresses. They are also what an nvmet
// target on a node advertises as its traddr.
func (c *Cluster) Addresses() []string { return c.addresses }

// reclaimWorkDir hands the work dir back to the current user so the unprivileged
// side of the harness can read the kubeconfig talosctl wrote as root.
func (c *Cluster) reclaimWorkDir(ctx context.Context) error {
	if !*c.cfg.Sudo {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	cmd := exec.CommandContext(ctx, "sudo", "chown", "-R", owner, c.workDir) //nolint:gosec // fixed binary
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reclaim %s: %w: %s", c.workDir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// run invokes talosctl, returning combined output because talosctl explains its
// failures there.
//
// -E preserves the environment: talosctl reads HOME for its image cache, and
// losing it would re-download the image on every run.
func (c *Cluster) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin, full := c.cfg.TalosctlPath, args
	if *c.cfg.Sudo {
		bin, full = "sudo", append([]string{"-E", c.cfg.TalosctlPath}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, full...) //nolint:gosec // fixed binary, structured args
	out, err := cmd.CombinedOutput()
	return string(out), err
}
