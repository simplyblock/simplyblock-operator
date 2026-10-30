package agent

// What the add-on installs on a member for the site link's agent, and the hub
// access its registration asks for.

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
)

// The agent runs the fleet image's agent binary, learns its site from OCM, and
// is reached by its site's peers on the site-link Service.
func TestTheAgentRunsAsItsSitesForwarder(t *testing.T) {
	var deploy *appsv1.Deployment
	var svc *corev1.Service
	for _, o := range Objects("example.test/fleet:1") {
		switch v := o.(type) {
		case *appsv1.Deployment:
			deploy = v
		case *corev1.Service:
			svc = v
		}
	}
	if deploy == nil || svc == nil {
		t.Fatalf("objects carry no Deployment (%v) or no Service (%v)", deploy != nil, svc != nil)
	}
	c := deploy.Spec.Template.Spec.Containers[0]
	if c.Image != "example.test/fleet:1" || len(c.Command) == 0 || c.Command[0] != "/fleet-agent" {
		t.Errorf("container runs %s %v, want the fleet image's /fleet-agent", c.Image, c.Command)
	}
	var site string
	for _, e := range c.Env {
		if e.Name == "CLUSTER_NAME" {
			site = e.Value
		}
	}
	if site != "{{CLUSTER_NAME}}" {
		t.Errorf("CLUSTER_NAME = %q, want OCM's {{CLUSTER_NAME}}", site)
	}
	if svc.Name != ServiceName || svc.Spec.Ports[0].Port != 9443 {
		t.Errorf("service %s:%d, want %s:9443", svc.Name, svc.Spec.Ports[0].Port, ServiceName)
	}
}

// The registration asks for the hub ClusterRole bound in the member's own
// namespace only.
func TestTheAgentsHubAccessIsItsOwnNamespace(t *testing.T) {
	reg := Registration()
	if len(reg) != 1 || reg[0].Type != addonv1alpha1.RegistrationTypeKubeClient || reg[0].KubeClient == nil {
		t.Fatalf("registration = %+v, want one KubeClient registration", reg)
	}
	perms := reg[0].KubeClient.HubPermissions
	if len(perms) != 1 || perms[0].Type != addonv1alpha1.HubPermissionsBindingCurrentCluster ||
		perms[0].CurrentCluster == nil || perms[0].CurrentCluster.ClusterRoleName != HubClusterRole {
		t.Errorf("hub permissions = %+v, want %s bound in the member's namespace", perms, HubClusterRole)
	}
}
