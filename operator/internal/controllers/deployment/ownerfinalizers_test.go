// Every kind the operator owns children of needs its finalizers granted.
//
// OwnerReferencesPermissionEnforcement is an admission plugin OpenShift enables
// by default and vanilla Kubernetes does not. Where it runs, setting
// blockOwnerDeletion on an ownerReference requires update on the owner's
// finalizers subresource — and controllerutil.SetControllerReference always sets
// that flag, so every owner the operator names needs the grant.
//
// Without it the create is refused, and the message names neither the missing
// permission nor the kind clearly:
//
//	storageclusterops ... is forbidden: cannot set blockOwnerDeletion if an
//	ownerReference refers to a resource you can't set finalizers on: , <nil>
//
// It passes on a cluster where the plugin is off, which is why this is a test
// against the generated role rather than something a K3s run would have caught.

package deployment

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// ownerKinds are the kinds the operator sets a controller reference to, which
// is the list every SetControllerReference call site resolves to. A new owner
// belongs here and in a marker, and this test is what says so.
var ownerKinds = []string{
	"clusterdeploymentconfigs", // the expansion's activate and its children
	"controlplanes",
	"simplyblockdrivers",
	"storageclusters",
	"storagenodes",
}

// managerRole is the ClusterRole controller-gen writes from the markers.
type managerRole struct {
	Rules []struct {
		APIGroups []string `json:"apiGroups"`
		Resources []string `json:"resources"`
		Verbs     []string `json:"verbs"`
	} `json:"rules"`
}

func TestEveryOwnerKindGrantsItsFinalizers(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatalf("read the generated manager role: %v", err)
	}
	var role managerRole
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatalf("parse the generated manager role: %v", err)
	}

	granted := map[string]bool{}
	for _, rule := range role.Rules {
		if !slices.Contains(rule.Verbs, "update") {
			continue
		}
		for _, resource := range rule.Resources {
			if strings.HasSuffix(resource, "/finalizers") {
				granted[resource] = true
			}
		}
	}

	for _, kind := range ownerKinds {
		if !granted[kind+"/finalizers"] {
			t.Errorf("%s is owned by something the operator creates and its finalizers are not granted, "+
				"so every child of it is refused where OwnerReferencesPermissionEnforcement runs", kind)
		}
	}
}
