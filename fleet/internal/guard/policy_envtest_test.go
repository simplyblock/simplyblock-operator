// The guard against a real API server.
//
// policy_cel_test.go proves the rule decides correctly. This proves the rest:
// that the policy installs, that its binding reaches it, that the match covers
// every resource of the storage group and no subresource, and that a refused
// request comes back as a refusal rather than as a rule nothing evaluated.
//
// The kind written against is declared here rather than borrowed from the
// operator. The guard matches the whole group, so any kind in it exercises the
// same path, and a kind of this suite's own keeps it independent of the
// operator's CustomResourceDefinition set and of the conversion stanza those
// carry, which envtest has nothing to serve.
//
// Identities come from impersonation. The envtest administrator holds
// system:masters and may impersonate, which is the only way one process can put
// several requesters in front of one policy.

package guard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	probeKind     = "GuardProbe"
	probePlural   = "guardprobes"
	probeVersion  = "v1alpha1"
	unguardedName = "unguarded.example.com"
)

var (
	probeGVR = schema.GroupVersionResource{Group: GuardedGroup, Version: probeVersion, Resource: probePlural}
	otherGVR = schema.GroupVersionResource{Group: unguardedName, Version: probeVersion, Resource: probePlural}
)

// guardEnv is one API server with the guard installed, shared by the tests in
// this file because starting one costs seconds and none of them mutates the
// policy.
type guardEnv struct {
	cfg *rest.Config
}

func startGuardEnv(t *testing.T) *guardEnv {
	t.Helper()

	assets := envTestAssets(t)
	environment := &envtest.Environment{BinaryAssetsDirectory: assets}

	cfg, err := environment.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	ctx := context.Background()
	installProbeCRDs(t, ctx, cfg)
	installGuard(t, ctx, cfg)

	return &guardEnv{cfg: cfg}
}

// envTestAssets locates the Kubernetes binaries, skipping when they are absent so
// that `go test ./...` without `make test` still runs the rest of the package.
func envTestAssets(t *testing.T) string {
	t.Helper()

	if assets := os.Getenv("KUBEBUILDER_ASSETS"); assets != "" {
		return assets
	}

	// The repository's shared .bin, which `make setup-envtest` populates.
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".bin", "k8s")
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) == 0 {
		t.Skip("no envtest assets: run `make -C fleet setup-envtest`")
	}
	return filepath.Join(root, entries[0].Name())
}

// installProbeCRDs declares one kind inside the guarded group and one outside
// it. The second is what proves the match is scoped to the group rather than to
// every custom resource.
func installProbeCRDs(t *testing.T, ctx context.Context, cfg *rest.Config) {
	t.Helper()

	crds := []*apiextensionsv1.CustomResourceDefinition{
		probeCRD(GuardedGroup),
		probeCRD(unguardedName),
	}
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{CRDs: crds}); err != nil {
		t.Fatalf("installing the probe CRDs: %v", err)
	}
	grantEveryone(t, ctx, cfg)
}

// grantEveryone gives every authenticated requester full access to the probe
// kinds, so that a refusal in this suite is the guard's and never RBAC's.
// Impersonating a user puts it in system:authenticated, which is what this binds.
func grantEveryone(t *testing.T, ctx context.Context, cfg *rest.Config) {
	t.Helper()

	clients := kubernetes.NewForConfigOrDie(cfg)
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "guard-probe-writer"},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{GuardedGroup, unguardedName},
				Resources: []string{probePlural, probePlural + "/status"},
				Verbs:     []string{"*"},
			},
			{
				// The guard's own objects. RBAC is granted to everyone here for
				// the same reason it is granted on the probes: a refusal in this
				// suite has to be the guard's and never RBAC's.
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"*"},
			},
			{
				APIGroups: []string{admissionregistrationv1.SchemeGroupVersion.Group},
				Resources: []string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"},
				Verbs:     []string{"*"},
			},
		},
	}
	if _, err := clients.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the probe role: %v", err)
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "guard-probe-writer"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects: []rbacv1.Subject{{
			APIGroup: rbacv1.GroupName,
			Kind:     "Group",
			Name:     "system:authenticated",
		}},
	}
	if _, err := clients.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil {
		t.Fatalf("binding the probe role: %v", err)
	}
}

func probeCRD(group string) *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: probePlural + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   probePlural,
				Singular: strings.ToLower(probeKind),
				Kind:     probeKind,
				ListKind: probeKind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    probeVersion,
				Served:  true,
				Storage: true,
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec":   {Type: "object", XPreserveUnknownFields: &preserve},
							"status": {Type: "object", XPreserveUnknownFields: &preserve},
						},
					},
				},
			}},
		},
	}
}

// installGuard applies the three objects the guard is made of, in the order
// Objects returns them, and waits until the policy actually refuses. A policy
// the API server has accepted is not yet a policy it evaluates, and a suite that
// skips the wait passes for the wrong reason.
func installGuard(t *testing.T, ctx context.Context, cfg *rest.Config) {
	t.Helper()

	clients := kubernetes.NewForConfigOrDie(cfg)
	if _, err := clients.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ParamsNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the parameter's namespace: %v", err)
	}
	if _, err := clients.CoreV1().ConfigMaps(ParamsNamespace).Create(ctx, Params(DefaultAllowlist()), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the allowlist: %v", err)
	}
	if _, err := clients.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, Policy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the policy: %v", err)
	}
	if _, err := clients.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, Binding(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the binding: %v", err)
	}
	if _, err := clients.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, SelfPolicy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the self policy: %v", err)
	}
	if _, err := clients.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, SelfBinding(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the self binding: %v", err)
	}

	waitUntil(t, 90*time.Second, "the guard never began refusing writes to "+GuardedGroup, func() error {
		err := createProbe(ctx, cfg, probeGVR, "guard-warmup", "kubernetes-admin", nil)
		if err == nil {
			_ = deleteProbe(ctx, cfg, probeGVR, "guard-warmup", "kubernetes-admin")
			return fmt.Errorf("the write was admitted")
		}
		if !apierrors.IsForbidden(err) {
			return err
		}
		return nil
	})

	// The self policy is a second sync, and every case that tries to remove the
	// guard is meaningless until it is live. A no-op update of the allowlist is
	// matched by it and changes nothing, so it is safe to retry.
	admin := asUser(cfg, "kubernetes-admin", []string{"system:masters"})
	waitUntil(t, 90*time.Second, "the self policy never began refusing", func() error {
		current, err := admin.CoreV1().ConfigMaps(ParamsNamespace).Get(ctx, Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		_, err = admin.CoreV1().ConfigMaps(ParamsNamespace).Update(ctx, current, metav1.UpdateOptions{})
		if err == nil {
			return fmt.Errorf("the update was admitted")
		}
		if !apierrors.IsForbidden(err) {
			return err
		}
		return nil
	})
}

// waitUntil retries a condition until it stops returning an error, which is how
// a suite waits for a policy the API server has accepted to become a policy it
// evaluates.
func waitUntil(t *testing.T, within time.Duration, what string, condition func() error) {
	t.Helper()

	deadline := time.Now().Add(within)
	var last error
	for {
		if last = condition(); last == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: last result %v", what, last)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// asUser returns a typed client acting as one requester, which is how the
// guard's own objects are reached now that only the fleet may change them.
func asUser(cfg *rest.Config, username string, groups []string) kubernetes.Interface {
	as := rest.CopyConfig(cfg)
	as.Impersonate = rest.ImpersonationConfig{UserName: username, Groups: groups}
	return kubernetes.NewForConfigOrDie(as)
}

// impersonating returns a dynamic client acting as one requester.
func impersonating(t *testing.T, cfg *rest.Config, username string, groups []string) dynamic.Interface {
	t.Helper()

	as := rest.CopyConfig(cfg)
	as.Impersonate = rest.ImpersonationConfig{UserName: username, Groups: groups}
	client, err := dynamic.NewForConfig(as)
	if err != nil {
		t.Fatalf("building a client for %q: %v", username, err)
	}
	return client
}

func probeObject(gvr schema.GroupVersionResource, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.Group + "/" + gvr.Version,
		"kind":       probeKind,
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec":       map[string]any{"value": "probe"},
	}}
}

func createProbe(ctx context.Context, cfg *rest.Config, gvr schema.GroupVersionResource, name, username string, groups []string) error {
	as := rest.CopyConfig(cfg)
	as.Impersonate = rest.ImpersonationConfig{UserName: username, Groups: groups}
	client, err := dynamic.NewForConfig(as)
	if err != nil {
		return fmt.Errorf("building a client: %w", err)
	}
	_, err = client.Resource(gvr).Namespace("default").Create(ctx, probeObject(gvr, name), metav1.CreateOptions{})
	return err
}

func deleteProbe(ctx context.Context, cfg *rest.Config, gvr schema.GroupVersionResource, name, username string) error {
	as := rest.CopyConfig(cfg)
	as.Impersonate = rest.ImpersonationConfig{UserName: username}
	client, err := dynamic.NewForConfig(as)
	if err != nil {
		return err
	}
	return client.Resource(gvr).Namespace("default").Delete(ctx, name, metav1.DeleteOptions{})
}

// TestGuardInAPIServer covers the decisions the guard makes about writes to the
// storage group, as subtests of one environment, because starting an API server
// per case would dominate the run.
func TestGuardInAPIServer(t *testing.T) {
	env := startGuardEnv(t)
	ctx := context.Background()

	t.Run("an allowed identity may create", func(t *testing.T) {
		if err := createProbe(ctx, env.cfg, probeGVR, "allowed-create", WorkAgentUser, nil); err != nil {
			t.Fatalf("the work agent was refused: %v", err)
		}
	})

	t.Run("a refused identity may not create, and the message names it", func(t *testing.T) {
		err := createProbe(ctx, env.cfg, probeGVR, "refused-create", "kubernetes-admin", []string{"system:masters"})
		if !apierrors.IsForbidden(err) {
			t.Fatalf("expected a refusal, got %v", err)
		}
		if !strings.Contains(err.Error(), "kubernetes-admin") {
			t.Errorf("the refusal does not name the requester: %v", err)
		}
	})

	t.Run("a group membership is enough", func(t *testing.T) {
		if err := createProbe(ctx, env.cfg, probeGVR, "group-create", "sre@simplyblock.io", []string{BreakGlassGroup}); err != nil {
			t.Fatalf("a break-glass identity was refused: %v", err)
		}
	})

	t.Run("another group is not guarded", func(t *testing.T) {
		if err := createProbe(ctx, env.cfg, otherGVR, "other-group", "kubernetes-admin", []string{"system:masters"}); err != nil {
			t.Fatalf("a write outside %s was refused: %v", GuardedGroup, err)
		}
	})

	t.Run("the garbage collector may create and delete", func(t *testing.T) {
		if err := createProbe(ctx, env.cfg, probeGVR, "collected", GarbageCollectorUser, nil); err != nil {
			t.Fatalf("the garbage collector was refused a create: %v", err)
		}
		if err := deleteProbe(ctx, env.cfg, probeGVR, "collected", GarbageCollectorUser); err != nil {
			t.Fatalf("the garbage collector was refused a delete, so a cascade down the ownership spine would stall: %v", err)
		}
	})

	t.Run("a refused identity may not delete", func(t *testing.T) {
		err := deleteProbe(ctx, env.cfg, probeGVR, "allowed-create", "kubernetes-admin")
		if !apierrors.IsForbidden(err) {
			t.Fatalf("expected a refusal on delete, got %v", err)
		}
	})

	t.Run("the status subresource is outside the match", func(t *testing.T) {
		client := impersonating(t, env.cfg, "kubernetes-admin", []string{"system:masters"})
		object, err := client.Resource(probeGVR).Namespace("default").Get(ctx, "allowed-create", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading the probe: %v", err)
		}
		_ = unstructured.SetNestedField(object.Object, "observed", "status", "phase")
		if _, err := client.Resource(probeGVR).Namespace("default").UpdateStatus(ctx, object, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("a status write by a refused identity was denied, so the match reaches subresources: %v", err)
		}
	})

	t.Run("a read by a refused identity is not admission's business", func(t *testing.T) {
		client := impersonating(t, env.cfg, "kubernetes-admin", []string{"system:masters"})
		if _, err := client.Resource(probeGVR).Namespace("default").List(ctx, metav1.ListOptions{}); err != nil {
			t.Fatalf("a list was refused, which admission cannot do: %v", err)
		}
	})
}

// TestGuardAllowlistInAPIServer covers the allowlist: who may change it, what
// happens when it is gone, and that being gone is recoverable. It takes an
// environment of its own because its cases delete the allowlist, which refuses
// every write in the environment they run in.
func TestGuardAllowlistInAPIServer(t *testing.T) {
	env := startGuardEnv(t)
	ctx := context.Background()

	t.Run("an administrator may not add themselves to the allowlist", func(t *testing.T) {
		admin := asUser(env.cfg, "kubernetes-admin", []string{"system:masters"})
		current, err := admin.CoreV1().ConfigMaps(ParamsNamespace).Get(ctx, Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading the allowlist: %v", err)
		}
		widened := current.DeepCopy()
		widened.Data["allowedUsers"] += ",\nkubernetes-admin"
		if _, err := admin.CoreV1().ConfigMaps(ParamsNamespace).Update(ctx, widened, metav1.UpdateOptions{}); !apierrors.IsForbidden(err) {
			t.Fatalf("the allowlist was editable by the identity it refuses: %v", err)
		}
	})

	t.Run("an administrator may not delete the allowlist and write a new one", func(t *testing.T) {
		admin := asUser(env.cfg, "kubernetes-admin", []string{"system:masters"})
		if err := admin.CoreV1().ConfigMaps(ParamsNamespace).Delete(ctx, Name, metav1.DeleteOptions{}); !apierrors.IsForbidden(err) {
			t.Fatalf("the allowlist was deletable, which is half of rewriting it: %v", err)
		}
		own := Params(Allowlist{Users: []string{"kubernetes-admin"}})
		own.Name = Name + "-mine"
		own.Name = Name
		if _, err := admin.CoreV1().ConfigMaps(ParamsNamespace).Create(ctx, own, metav1.CreateOptions{}); !apierrors.IsForbidden(err) && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("an allowlist of the requester's own was creatable: %v", err)
		}
	})

	t.Run("a break-glass operator may change the guard", func(t *testing.T) {
		sre := asUser(env.cfg, "sre@simplyblock.io", []string{BreakGlassGroup})
		current, err := sre.CoreV1().ConfigMaps(ParamsNamespace).Get(ctx, Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading the allowlist: %v", err)
		}
		touched := current.DeepCopy()
		touched.Annotations = map[string]string{"simplyblock.io/touched-by": "break-glass"}
		if _, err := sre.CoreV1().ConfigMaps(ParamsNamespace).Update(ctx, touched, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("break-glass could not reach the guard, leaving deletion as the only lever: %v", err)
		}
	})

	t.Run("removing the allowlist refuses every write", func(t *testing.T) {
		fleet := asUser(env.cfg, WorkAgentUser, nil)
		if err := fleet.CoreV1().ConfigMaps(ParamsNamespace).Delete(ctx, Name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("deleting the allowlist: %v", err)
		}
		t.Cleanup(func() {
			_, _ = fleet.CoreV1().ConfigMaps(ParamsNamespace).Create(ctx, Params(DefaultAllowlist()), metav1.CreateOptions{})
		})

		// The refusal here is the binding failing to configure rather than the
		// rule deciding, so it does not carry the Forbidden reason a validation
		// denial does. What matters is that the write is refused and that the
		// guard is what refused it.
		deadline := time.Now().Add(60 * time.Second)
		for {
			err := createProbe(ctx, env.cfg, probeGVR, "no-params", WorkAgentUser, nil)
			if err != nil {
				if !strings.Contains(err.Error(), Name) {
					t.Fatalf("the write was refused by something other than the guard: %v", err)
				}
				return
			}
			_ = deleteProbe(ctx, env.cfg, probeGVR, "no-params", WorkAgentUser)
			if time.Now().After(deadline) {
				t.Fatal("an absent allowlist admitted a write")
			}
			time.Sleep(250 * time.Millisecond)
		}
	})

	t.Run("a member whose allowlist is gone is recoverable", func(t *testing.T) {
		fleet := asUser(env.cfg, WorkAgentUser, nil)
		if err := fleet.CoreV1().ConfigMaps(ParamsNamespace).Delete(ctx, Name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("deleting the allowlist: %v", err)
		}

		// The storage group is refused while the allowlist is missing, which is
		// the correct direction. What must not also be true is that restoring it
		// is refused, because the allowlist is one of the objects the self
		// policy guards and the self policy has no allowlist of its own to lose.
		if _, err := fleet.CoreV1().ConfigMaps(ParamsNamespace).Create(ctx, Params(DefaultAllowlist()), metav1.CreateOptions{}); err != nil {
			t.Fatalf("the fleet could not restore a deleted allowlist, so the member is deadlocked: %v", err)
		}

		deadline := time.Now().Add(60 * time.Second)
		for {
			err := createProbe(ctx, env.cfg, probeGVR, "recovered", WorkAgentUser, nil)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("writes did not resume after the allowlist was restored: %v", err)
			}
			time.Sleep(250 * time.Millisecond)
		}
	})

	t.Run("an audit-only binding admits what it records", func(t *testing.T) {
		clients := asUser(env.cfg, WorkAgentUser, nil)
		binding := Binding(admissionregistrationv1.Audit)
		existing, err := clients.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(ctx, Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading the binding: %v", err)
		}
		binding.ResourceVersion = existing.ResourceVersion
		if _, err := clients.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Update(ctx, binding, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("switching the binding to audit: %v", err)
		}
		t.Cleanup(func() {
			current, err := clients.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(ctx, Name, metav1.GetOptions{})
			if err != nil {
				return
			}
			restored := Binding()
			restored.ResourceVersion = current.ResourceVersion
			_, _ = clients.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Update(ctx, restored, metav1.UpdateOptions{})
		})

		deadline := time.Now().Add(60 * time.Second)
		for {
			err := createProbe(ctx, env.cfg, probeGVR, "audit-only", "kubernetes-admin", []string{"system:masters"})
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("an audit-only binding still refused: %v", err)
			}
			time.Sleep(250 * time.Millisecond)
		}
	})
}

// TestAdmissionCannotGuardAdmissionConfiguration pins the ceiling on everything
// above it.
//
// Kubernetes skips admission on admissionregistration.k8s.io resources to avoid
// circular dependencies, so no policy and no webhook intercepts its own
// deletion. A sufficiently privileged user in the member removes the guard and
// nothing in the admission chain stops them, which is why the fleet's answer to
// a removed guard is repair and alarm rather than prevention.
//
// The test runs in an environment of its own because it destroys the guard, and
// it asserts the behavior rather than wishing for it, so that a rule matching
// these resources is not added back on the assumption that it would fire.
func TestAdmissionCannotGuardAdmissionConfiguration(t *testing.T) {
	env := startGuardEnv(t)
	ctx := context.Background()
	admin := asUser(env.cfg, "kubernetes-admin", []string{"system:masters"})

	if err := admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(ctx, Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting the guard's binding: %v", err)
	}

	// With the binding gone the rule no longer runs, and the identity it refused
	// writes freely. This is the window a ManifestWork's re-apply closes after
	// the fact, and does not prevent.
	waitUntil(t, 60*time.Second, "the guard kept refusing after its binding was deleted", func() error {
		return createProbe(ctx, env.cfg, probeGVR, "after-removal", "kubernetes-admin", []string{"system:masters"})
	})
}
