// Package agent defines what the fleet add-on installs on a member for the site
// link's agent (design-stretched-sync-replication-ramen.md §7.3), and the hub
// access its registration asks for. hack/gen-addon renders both into the
// add-on template, beside the admission guard internal/guard defines, so what
// the tests check is what a member receives.
package agent

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/fleet/internal/sitelink"
)

const (
	// Namespace is where the add-on's agents run on a member, the namespace
	// Open Cluster Management installs add-ons to.
	Namespace = "open-cluster-management-agent-addon"
	// Name is the agent's Deployment, ServiceAccount, and RBAC.
	Name = "simplyblock-fleet-agent"
	// ServiceName is the Service a site's operator and CSI controller reach
	// the agent by.
	ServiceName = "simplyblock-site-link"
	// HubClusterRole is the ClusterRole on the hub the registration binds in
	// the member's own namespace: read the relay endpoint, request the link
	// token, and nothing else.
	HubClusterRole = "simplyblock-site-link-agent"
	// port is where the agent listens for its site's peers.
	port = 9443
)

// Objects are the agent's objects on a member, running image.
func Objects(image string) []client.Object {
	labels := map[string]string{"app": Name}
	return []client.Object{
		&corev1.ServiceAccount{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
			ObjectMeta: metav1.ObjectMeta{Name: Name, Namespace: Namespace},
		},
		// The local hub verifies its site's peers with TokenReviews.
		&rbacv1.ClusterRole{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
			ObjectMeta: metav1.ObjectMeta{Name: Name},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"},
			}},
		},
		&rbacv1.ClusterRoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: Name},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: Name},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: Name, Namespace: Namespace}},
		},
		// The agent publishes its serving CA in its own namespace.
		&rbacv1.Role{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
			ObjectMeta: metav1.ObjectMeta{Name: Name, Namespace: Namespace},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "create", "update"},
			}},
		},
		&rbacv1.RoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: Name, Namespace: Namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: Name},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: Name, Namespace: Namespace}},
		},
		// The site's peers read the agent's CA from this namespace.
		&rbacv1.Role{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
			ObjectMeta: metav1.ObjectMeta{Name: Name + "-ca-reader", Namespace: Namespace},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"configmaps"},
				ResourceNames: []string{sitelink.LocalCAConfigMap}, Verbs: []string{"get"},
			}},
		},
		&rbacv1.RoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: Name + "-ca-reader", Namespace: Namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: Name + "-ca-reader"},
			Subjects:   []rbacv1.Subject{{Kind: "Group", Name: "system:serviceaccounts", APIGroup: "rbac.authorization.k8s.io"}},
		},
		&corev1.Service{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
			ObjectMeta: metav1.ObjectMeta{Name: ServiceName, Namespace: Namespace},
			Spec: corev1.ServiceSpec{
				Selector: labels,
				Ports:    []corev1.ServicePort{{Name: "site-link", Port: port, TargetPort: intstr.FromInt32(port)}},
			},
		},
		&appsv1.Deployment{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			ObjectMeta: metav1.ObjectMeta{Name: Name, Namespace: Namespace, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						ServiceAccountName: Name,
						Containers: []corev1.Container{{
							Name:    "agent",
							Image:   image,
							Command: []string{"/fleet-agent"},
							Args:    []string{"--hub-kubeconfig={{HUB_KUBECONFIG}}"},
							Env: []corev1.EnvVar{
								{Name: "CLUSTER_NAME", Value: "{{CLUSTER_NAME}}"},
								{Name: "POD_NAMESPACE", ValueFrom: fieldRef("metadata.namespace")},
								{Name: "POD_UID", ValueFrom: fieldRef("metadata.uid")},
							},
							Ports: []corev1.ContainerPort{{Name: "site-link", ContainerPort: port}},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptr.To(false),
								RunAsNonRoot:             ptr.To(true),
								Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							},
						}},
					},
				},
			},
		},
	}
}

// Registration asks for a hub credential bound to HubClusterRole in the
// member's own namespace.
func Registration() []addonv1alpha1.RegistrationSpec {
	return []addonv1alpha1.RegistrationSpec{{
		Type: addonv1alpha1.RegistrationTypeKubeClient,
		KubeClient: &addonv1alpha1.KubeClientRegistrationConfig{
			HubPermissions: []addonv1alpha1.HubPermissionConfig{{
				Type:           addonv1alpha1.HubPermissionsBindingCurrentCluster,
				CurrentCluster: &addonv1alpha1.CurrentClusterBindingConfig{ClusterRoleName: HubClusterRole},
			}},
		},
	}}
}

func fieldRef(path string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: path}}
}
