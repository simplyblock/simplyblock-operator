//go:build e2e
// +build e2e

/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/simplyblock/simplyblock-operator/test/utils"
)

// RBAC end-to-end coverage for the simplyblock operator's user-facing access
// model:
//
//   * the aggregation ClusterRoles correctly fold simplyblock CR verbs into
//     the built-in Kubernetes view/edit ClusterRoles, so namespace tenants get
//     the expected level of access without bespoke bindings;
//   * a resourceNames-scoped Role grants get/update/patch/delete only for the
//     named StorageCluster, and notably does NOT grant list/watch/create on
//     the type — a built-in K8s RBAC limitation we document and want to make
//     visible if it ever changes.
//
// The Pool same-namespace enforcement is exercised by the unit test
// TestPoolReconcileRejectsCrossNamespaceClusterReference, so we don't repeat
// the full reconcile loop here; that keeps this suite independent of operator
// pod readiness and runnable on just a Kind cluster with CRDs + RBAC applied.
//
// Setup is idempotent and self-contained, so this Describe can run before or
// after the Manager Describe regardless of Ginkgo's randomized container order.

const (
	rbacFooNS                 = "rbac-cluster-foo"
	rbacBarNS                 = "rbac-cluster-bar"
	rbacViewerSA              = "viewer-sa"
	rbacEditorSA              = "editor-sa"
	rbacOutsiderSA            = "outsider-sa"
	rbacScopedSA              = "scoped-sa"
	rbacScopedRoleName        = "rbac-foo-admin"
	rbacScopedClusterAllowed  = "rbac-allowed"
	rbacScopedClusterForbidden = "rbac-forbidden"
)

var _ = Describe("RBAC", Ordered, func() {
	BeforeAll(func() {
		By("ensuring CRDs are installed")
		_, err := utils.Run(exec.Command("make", "install"))
		Expect(err).NotTo(HaveOccurred(), "failed to install CRDs")

		By("applying the aggregation ClusterRoles")
		// These are part of the operator's config/rbac and would also be installed
		// by `make deploy`. We apply them directly so this Describe is independent
		// of whether the operator deployment is currently up.
		for _, path := range []string{
			"config/rbac/aggregate_view_role.yaml",
			"config/rbac/aggregate_edit_role.yaml",
		} {
			_, err := utils.Run(exec.Command("kubectl", "apply", "-f", path))
			Expect(err).NotTo(HaveOccurred(), "failed to apply %s", path)
		}

		By("creating test namespaces")
		for _, ns := range []string{rbacFooNS, rbacBarNS} {
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", ns))
			Expect(err).NotTo(HaveOccurred(), "failed to create namespace %s", ns)
		}

		By("creating ServiceAccounts in the foo namespace")
		for _, sa := range []string{rbacViewerSA, rbacEditorSA, rbacOutsiderSA, rbacScopedSA} {
			_, err := utils.Run(exec.Command("kubectl", "create", "sa", sa, "-n", rbacFooNS))
			Expect(err).NotTo(HaveOccurred(), "failed to create SA %s", sa)
		}

		By("binding the built-in view ClusterRole to the viewer SA")
		_, err = utils.Run(exec.Command("kubectl", "create", "rolebinding", "viewer-binding",
			"--clusterrole=view",
			"--serviceaccount="+rbacFooNS+":"+rbacViewerSA,
			"-n", rbacFooNS))
		Expect(err).NotTo(HaveOccurred(), "failed to bind view role")

		By("binding the built-in edit ClusterRole to the editor SA")
		_, err = utils.Run(exec.Command("kubectl", "create", "rolebinding", "editor-binding",
			"--clusterrole=edit",
			"--serviceaccount="+rbacFooNS+":"+rbacEditorSA,
			"-n", rbacFooNS))
		Expect(err).NotTo(HaveOccurred(), "failed to bind edit role")

		By("applying a resourceNames-scoped Role and binding it to the scoped SA")
		scopedManifest := fmt.Sprintf(`
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: %s
  namespace: %s
rules:
- apiGroups: ["storage.simplyblock.io"]
  resources: ["storageclusters"]
  resourceNames: ["%s"]
  verbs: ["get", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: scoped-binding
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: %s
subjects:
- kind: ServiceAccount
  name: %s
  namespace: %s
`, rbacScopedRoleName, rbacFooNS, rbacScopedClusterAllowed, rbacFooNS, rbacScopedRoleName, rbacScopedSA, rbacFooNS)
		applyCmd := exec.Command("kubectl", "apply", "-f", "-")
		applyCmd.Stdin = strings.NewReader(scopedManifest)
		_, err = utils.Run(applyCmd)
		Expect(err).NotTo(HaveOccurred(), "failed to apply scoped Role/RoleBinding")
	})

	AfterAll(func() {
		By("deleting test namespaces")
		for _, ns := range []string{rbacFooNS, rbacBarNS} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", ns, "--ignore-not-found"))
		}
		// The aggregation ClusterRoles and CRDs are install-level artifacts; leave
		// them in place so they can be reused by other Describes.
	})

	Context("aggregation into built-in view/edit/admin", func() {
		It("grants viewer SA get/list/watch on simplyblock CRs in its namespace", func() {
			expectCanI(rbacFooNS, rbacViewerSA, "get", "storageclusters.storage.simplyblock.io", "", true)
			expectCanI(rbacFooNS, rbacViewerSA, "list", "storageclusters.storage.simplyblock.io", "", true)
			expectCanI(rbacFooNS, rbacViewerSA, "watch", "pools.storage.simplyblock.io", "", true)
		})

		It("denies viewer SA mutations", func() {
			expectCanI(rbacFooNS, rbacViewerSA, "create", "storageclusters.storage.simplyblock.io", "", false)
			expectCanI(rbacFooNS, rbacViewerSA, "patch", "pools.storage.simplyblock.io", "", false)
		})

		It("denies viewer SA access to other namespaces", func() {
			expectCanI(rbacBarNS, rbacViewerSA, "get", "storageclusters.storage.simplyblock.io", "", false)
		})

		It("grants editor SA full CRUD on simplyblock CRs in its namespace", func() {
			for _, verb := range []string{"get", "list", "create", "update", "patch", "delete"} {
				expectCanI(rbacFooNS, rbacEditorSA, verb, "storageclusters.storage.simplyblock.io", "", true)
				expectCanI(rbacFooNS, rbacEditorSA, verb, "pools.storage.simplyblock.io", "", true)
			}
		})

		It("denies editor SA access to other namespaces", func() {
			expectCanI(rbacBarNS, rbacEditorSA, "create", "pools.storage.simplyblock.io", "", false)
		})

		It("denies outsider SA all access", func() {
			expectCanI(rbacFooNS, rbacOutsiderSA, "get", "storageclusters.storage.simplyblock.io", "", false)
			expectCanI(rbacFooNS, rbacOutsiderSA, "list", "pools.storage.simplyblock.io", "", false)
		})
	})

	Context("resourceNames-scoped Role", func() {
		It("grants the named verbs on the allowed StorageCluster only", func() {
			expectCanI(rbacFooNS, rbacScopedSA, "get", "storageclusters.storage.simplyblock.io", rbacScopedClusterAllowed, true)
			expectCanI(rbacFooNS, rbacScopedSA, "patch", "storageclusters.storage.simplyblock.io", rbacScopedClusterAllowed, true)
			expectCanI(rbacFooNS, rbacScopedSA, "delete", "storageclusters.storage.simplyblock.io", rbacScopedClusterAllowed, true)
		})

		It("does not grant access to a different StorageCluster name", func() {
			expectCanI(rbacFooNS, rbacScopedSA, "get", "storageclusters.storage.simplyblock.io", rbacScopedClusterForbidden, false)
		})

		It("does not grant list (a documented K8s RBAC limitation)", func() {
			// K8s RBAC's `resourceNames` filter applies only to verbs that target a
			// named object (get/update/patch/delete). For list/watch/create the
			// filter is ignored, so a resourceNames-only Role cannot grant them.
			// We assert this behaviour here so we notice if it ever changes.
			expectCanI(rbacFooNS, rbacScopedSA, "list", "storageclusters.storage.simplyblock.io", "", false)
			expectCanI(rbacFooNS, rbacScopedSA, "watch", "storageclusters.storage.simplyblock.io", "", false)
		})
	})
})

// expectCanI runs `kubectl auth can-i <verb> <resource>[/<name>] -n <ns>
// --as=system:serviceaccount:<ns>:<sa>` and asserts the answer matches `want`.
//
// kubectl reports "yes" with exit code 0 when permission is granted and "no"
// (with exit code 1) otherwise; both are expected outputs, so we look at the
// printed answer rather than treating exit code 1 as a test failure.
func expectCanI(namespace, sa, verb, resource, resourceName string, want bool) {
	GinkgoHelper()

	target := resource
	if resourceName != "" {
		target = resource + "/" + resourceName
	}

	cmd := exec.Command("kubectl", "auth", "can-i",
		verb, target,
		"-n", namespace,
		"--as="+"system:serviceaccount:"+namespace+":"+sa,
	)
	out, _ := cmd.CombinedOutput()
	answer := strings.TrimSpace(string(out))

	switch want {
	case true:
		Expect(answer).To(Equal("yes"),
			"expected SA %q to be allowed to %q %q in %q, got %q", sa, verb, target, namespace, answer)
	case false:
		Expect(answer).To(Equal("no"),
			"expected SA %q to be denied %q %q in %q, got %q", sa, verb, target, namespace, answer)
	}
}
