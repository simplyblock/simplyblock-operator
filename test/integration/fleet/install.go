// Installing the fleet onto the hub, and getting its add-on into the member.
//
// Everything applied here is the committed artifact rather than something this
// suite composes. The point of the suite is that what a deployment applies is
// what enforces, so a manifest generated for the test would be testing a
// different thing.
//
// One storage-group CustomResourceDefinition is installed alongside, because the
// guard covers that group and a group with no kind in it admits nothing to
// refuse. OperatorOps is the one chosen: it carries no conversion webhook, which
// there is nothing here to serve, and a valid object is one field.

package fleet

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/test/integration/cluster"
)

const (
	// guardName is the name the guard's policy, binding, and allowlist share.
	guardName = "simplyblock-storage-guard"

	// guardSelfName is the policy that keeps the allowlist from being rewritten.
	guardSelfName = "simplyblock-storage-guard-self"

	// addOnName is the add-on that carries the guard.
	addOnName = "simplyblock-fleet"

	// addOnNamespace is where the add-on's objects land in the member, and where
	// the allowlist the policy reads has to be for the binding to resolve it.
	addOnNamespace = "open-cluster-management-agent-addon"

	// guardedGroup is what the guard covers.
	guardedGroup = "storage.simplyblock.io"
)

// repoRoot locates the repository from this file's own path, so the suite finds
// the manifests wherever it is run from.
func repoRoot(t *testing.T) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this file, so the repository root is unknown")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

// grantWorkAgent applies the member-side prerequisite.
//
// Open Cluster Management's work agent applies the add-on's payload under its
// own identity, and that identity reaches nothing in
// admissionregistration.k8s.io, so without this every policy and binding in the
// payload is refused and the guard never arrives. The fleet cannot ship the
// grant itself, because a ClusterRole conferring permissions its creator lacks
// is refused, so it is applied in the member by whoever enrolls it. Here that is
// the suite, standing in for that person.
func grantWorkAgent(ctx context.Context, t *testing.T, c *cluster.Cluster) {
	t.Helper()

	path := filepath.Join(repoRoot(t), "fleet", "config", "member")
	if out, err := c.Kubectl(ctx, "apply", "-k", path); err != nil {
		t.Fatalf("granting the work agent its admission permissions: %v\n%s", err, out)
	}
}

// installFleet applies the fleet's own kinds, the add-on, and the one storage
// kind the guard is given something to guard.
func installFleet(ctx context.Context, t *testing.T, c *cluster.Cluster) {
	t.Helper()

	root := repoRoot(t)
	for _, path := range []string{
		filepath.Join(root, "fleet", "config", "crd", "bases"),
		filepath.Join(root, "operator", "config", "crd", "bases", "storage.simplyblock.io_operatorops.yaml"),
	} {
		if out, err := c.Kubectl(ctx, "apply", "-f", path); err != nil {
			t.Fatalf("applying %s: %v\n%s", path, err, out)
		}
	}

	addon := filepath.Join(root, "fleet", "config", "addon")
	if out, err := c.Kubectl(ctx, "apply", "-k", addon); err != nil {
		t.Fatalf("applying the add-on: %v\n%s", err, out)
	}
}

// grantProbeAccess gives every authenticated requester RBAC on the kinds this
// suite writes, so that a refusal is the guard's and never RBAC's. The
// identities under test arrive by impersonation, which puts them in
// system:authenticated.
func grantProbeAccess(ctx context.Context, t *testing.T, c *cluster.Cluster) {
	t.Helper()

	manifest := `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: fleet-suite-probe
rules:
  - apiGroups: ["` + guardedGroup + `"]
    resources: ["*"]
    verbs: ["*"]
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["*"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: fleet-suite-probe
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: fleet-suite-probe
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: system:authenticated
`
	if err := c.Apply(ctx, manifest); err != nil {
		t.Fatalf("granting the suite's probe access: %v", err)
	}
}

// installAddOn asks for the add-on in the member and waits for what it carries
// to arrive.
//
// The install strategy is Manual, so this is the step the fleet manager performs
// per FleetMember once it exists, and until then a suite performs it the same
// way a person would.
func installAddOn(ctx context.Context, t *testing.T, c *cluster.Cluster) {
	t.Helper()

	manifest := `
apiVersion: addon.open-cluster-management.io/v1alpha1
kind: ManagedClusterAddOn
metadata:
  name: ` + addOnName + `
  namespace: ` + memberName + `
spec:
  installNamespace: ` + addOnNamespace + `
`
	if err := c.Apply(ctx, manifest); err != nil {
		t.Fatalf("asking for the add-on: %v", err)
	}

	diagnose := func() string { return addOnDiagnostics(ctx, c) }

	waitFor(ctx, t, 5*time.Minute, "the add-on produced no ManifestWork", func() error {
		out, err := c.Kubectl(ctx, "-n", memberName, "get", "manifestwork",
			"-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "" {
			return fmt.Errorf("no ManifestWork exists yet")
		}
		return nil
	}, diagnose)

	waitFor(ctx, t, 5*time.Minute, "the guard's policy never reached the member", func() error {
		_, err := c.Kubectl(ctx, "get", "validatingadmissionpolicy", guardName)
		return err
	}, diagnose)
}

// addOnDiagnostics is what a failed delivery owes the reader.
//
// A ManifestWork nothing has applied carries no status at all, which reads as
// silence rather than as a fault, and the fault is usually one pod away: an
// agent that never scheduled applies nothing, and on a node sized for one half
// of Open Cluster Management that is where the chain breaks first.
func addOnDiagnostics(ctx context.Context, c *cluster.Cluster) string {
	probes := []struct {
		what string
		args []string
	}{
		{"add-on", []string{"-n", memberName, "get", "managedclusteraddon", addOnName,
			"-o", `jsonpath={range .status.conditions[*]}{.type}={.status} ({.reason}): {.message}{"\n"}{end}`}},
		{"ManifestWork", []string{"-n", memberName, "get", "manifestwork",
			"-o", `jsonpath={range .items[*]}{.metadata.name}: {range .status.conditions[*]}{.type}={.status} ({.reason}) {end}{"\n"}{end}`}},
		{"pods that are not running", []string{"get", "pods", "-A",
			"--field-selector", "status.phase!=Running"}},
		{"agent scheduling", []string{"-n", "open-cluster-management-agent", "get", "pods",
			"-o", `jsonpath={range .items[*]}{.metadata.name}: {range .status.conditions[?(@.type=="PodScheduled")]}{.status} {.reason} {.message}{end}{"\n"}{end}`}},
	}

	var report strings.Builder
	for _, probe := range probes {
		out, err := c.Kubectl(ctx, probe.args...)
		if err != nil {
			out = err.Error()
		}
		report.WriteString("  " + probe.what + ":\n")
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			report.WriteString("    " + line + "\n")
		}
	}
	return report.String()
}

// asUser runs kubectl as one requester through impersonation.
func asUser(ctx context.Context, c *cluster.Cluster, username string, groups []string, args ...string) (string, error) {
	full := append([]string{"--as", username}, args...)
	for _, group := range groups {
		full = append([]string{"--as-group", group}, full...)
	}
	return c.Kubectl(ctx, full...)
}

// applyAs pipes a manifest through kubectl as one requester. It is separate from
// asUser because Apply reads its manifest from stdin, and impersonation flags
// have to precede the subcommand either way.
func applyAs(ctx context.Context, c *cluster.Cluster, username string, groups []string, manifest string) (string, error) {
	args := []string{"--as", username}
	for _, group := range groups {
		args = append(args, "--as-group", group)
	}
	args = append(args, "apply", "-f", "-")
	return c.KubectlStdin(ctx, manifest, args...)
}

// operatorOps is a valid object of the guarded group, as small as that group
// allows: one required field carrying its one permitted value.
func operatorOps(name string) string {
	return `
apiVersion: ` + guardedGroup + `/v1alpha2
kind: OperatorOps
metadata:
  name: ` + name + `
  namespace: default
spec:
  action: Discover
`
}
