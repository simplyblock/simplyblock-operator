package cluster

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Kubernetes access goes through kubectl rather than client-go. The harness needs
// apply, exec and a few waits; client-go would be the right answer once it needs
// watches or informers, and swapping it in is confined to this file.

// Runs the Kubernetes CLI against this cluster and returns combined output.
func (c *Cluster) Kubectl(ctx context.Context, args ...string) (string, error) {
	return c.kubectl(ctx, 2*time.Minute, args...)
}

func (c *Cluster) kubectl(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append([]string{"--kubeconfig", c.kubeconfig}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...) //nolint:gosec // fixed binary, structured args
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("kubectl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Apply pipes a manifest to kubectl apply.
func (c *Cluster) Apply(ctx context.Context, manifest string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", c.kubeconfig, "apply", "-f", "-") //nolint:gosec // fixed binary
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Delete removes a manifest's objects, ignoring ones already gone so it is safe
// from a deferred cleanup.
func (c *Cluster) Delete(ctx context.Context, manifest string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", c.kubeconfig, //nolint:gosec // fixed binary
		"delete", "--ignore-not-found", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("delete: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Nodes returns the cluster's node names, in the order the API reports them.
func (c *Cluster) Nodes(ctx context.Context) ([]string, error) {
	out, err := c.Kubectl(ctx, "get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// WaitNodesReady blocks until every node reports Ready. A Talos cluster answers
// its API before the nodes are schedulable, so this is not the same as the
// cluster existing.
func (c *Cluster) WaitNodesReady(ctx context.Context, expected int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out, err := c.Kubectl(ctx, "get", "nodes",
			"-o", "jsonpath={range .items[*]}{.metadata.name}={.status.conditions[?(@.type=='Ready')].status} {end}")
		if err == nil {
			last = strings.TrimSpace(out)
			ready := strings.Count(last, "=True")
			if ready >= expected {
				return nil
			}
		} else {
			last = err.Error()
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("only %q ready after %s, wanted %d nodes", last, timeout, expected)
}

// WaitPodReady blocks until a pod is Ready.
func (c *Cluster) WaitPodReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	out, err := c.kubectl(ctx, timeout+30*time.Second,
		"wait", "--for=condition=Ready", "pod/"+name, "-n", namespace,
		fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
	if err != nil {
		desc, _ := c.Kubectl(ctx, "describe", "pod", name, "-n", namespace)
		return fmt.Errorf("%w\n%s\n--- describe ---\n%s", err, out, tailLines(desc, 30))
	}
	return nil
}

// Exec runs a command in a pod and returns its combined output.
func (c *Cluster) Exec(ctx context.Context, namespace, pod, script string) (string, error) {
	return c.kubectl(ctx, 3*time.Minute,
		"exec", "-n", namespace, pod, "--", "/bin/sh", "-c", script)
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
