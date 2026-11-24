package bootstrap

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const ns = "simplyblock"

const controlPlaneYAML = `# a comment the chart carries along
apiVersion: storage.simplyblock.io/v1alpha2
kind: ControlPlane
metadata:
  name: simplyblock
  namespace: simplyblock
  annotations:
    helm.sh/resource-policy: keep
spec:
  source:
    managed:
      endpoint: "https://cp.example.com"
`

const driverYAML = `apiVersion: storage.simplyblock.io/v1alpha2
kind: SimplyblockDriver
metadata:
  name: simplyblock
  namespace: simplyblock
spec:
  driverName: "csi.simplyblock.io"
  controllerReplicas: 1
`

const discoveryYAML = `apiVersion: storage.simplyblock.io/v1alpha2
kind: OperatorOps
metadata:
  name: initial-discovery
  namespace: simplyblock
spec:
  action: Discover
  discover:
    enablePartitionedDevices: true
`

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := simplyblockv1alpha2.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func configMap(data map[string]string, annotations map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: ConfigMapName, Namespace: ns, UID: "cm-uid", Annotations: annotations,
		},
		Data: data,
	}
}

func reconcileOnce(t *testing.T, r *Reconciler) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: ConfigMapName},
	})
	return err
}

func appliedRecord(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}
	raw, ok := cm.Annotations[AppliedAnnotation]
	if !ok {
		return nil
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func exists(t *testing.T, c client.Client, obj client.Object, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func TestAppliesEveryObjectOfTheConfigMap(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(configMap(map[string]string{
		"controlplane.yaml":      controlPlaneYAML,
		"simplyblockdriver.yaml": driverYAML,
		"initial-discovery.yaml": discoveryYAML,
	}, map[string]string{ReleaseIsInstallAnnotation: "true"})).Build()
	r := &Reconciler{Client: c, Namespace: ns}

	if err := reconcileOnce(t, r); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var cp simplyblockv1alpha2.ControlPlane
	if !exists(t, c, &cp, "simplyblock") {
		t.Fatal("the ControlPlane was not applied")
	}
	if cp.Spec.Source.Managed == nil || cp.Spec.Source.Managed.Endpoint != "https://cp.example.com" {
		t.Fatalf("the ControlPlane's spec was not applied: %+v", cp.Spec.Source)
	}
	var drv simplyblockv1alpha2.SimplyblockDriver
	if !exists(t, c, &drv, "simplyblock") {
		t.Fatal("the SimplyblockDriver was not applied")
	}
	var ops simplyblockv1alpha2.OperatorOps
	if !exists(t, c, &ops, "initial-discovery") {
		t.Fatal("the discovery run was not applied")
	}
	if refs := ops.GetOwnerReferences(); len(refs) != 1 || refs[0].Kind != "ConfigMap" || refs[0].UID != "cm-uid" {
		t.Fatalf("the discovery run should be owned by the ConfigMap, got %+v", refs)
	}
	if refs := cp.GetOwnerReferences(); len(refs) != 0 {
		t.Fatalf("the ControlPlane must not be owned by the ConfigMap, got %+v", refs)
	}

	rec := appliedRecord(t, c)
	if len(rec) != 3 || rec["controlplane.yaml"] != digestOf(controlPlaneYAML) {
		t.Fatalf("applied record = %v", rec)
	}
}

func TestRetriesWhileTheKindIsNotServed(t *testing.T) {
	notServed := 2
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(configMap(map[string]string{
		"controlplane.yaml": controlPlaneYAML,
	}, nil)).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if notServed > 0 {
				notServed--
				return &meta.NoKindMatchError{
					GroupKind:        schema.GroupKind{Group: group, Kind: "ControlPlane"},
					SearchedVersions: []string{"v1alpha2"},
				}
			}
			return cl.Apply(ctx, obj, opts...)
		},
	}).Build()
	r := &Reconciler{Client: c, Namespace: ns}

	for i := 0; i < 2; i++ {
		err := reconcileOnce(t, r)
		if err == nil || !meta.IsNoMatchError(err) {
			t.Fatalf("attempt %d: want a no-kind-match error so the request is retried, got %v", i, err)
		}
		if _, ok := appliedRecord(t, c)["controlplane.yaml"]; ok {
			t.Fatalf("attempt %d: an object that was not applied is recorded as applied", i)
		}
	}
	if err := reconcileOnce(t, r); err != nil {
		t.Fatalf("once the kind is served the apply should succeed, got %v", err)
	}
	if !exists(t, c, &simplyblockv1alpha2.ControlPlane{}, "simplyblock") {
		t.Fatal("the ControlPlane was not applied once served")
	}
}

func TestRemovingAKeyDoesNotDeleteTheObject(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(configMap(map[string]string{
		"controlplane.yaml": controlPlaneYAML,
	}, nil)).Build()
	r := &Reconciler{Client: c, Namespace: ns}
	if err := reconcileOnce(t, r); err != nil {
		t.Fatal(err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data = map[string]string{}
	if err := c.Update(context.Background(), &cm); err != nil {
		t.Fatal(err)
	}
	if err := reconcileOnce(t, r); err != nil {
		t.Fatal(err)
	}

	if !exists(t, c, &simplyblockv1alpha2.ControlPlane{}, "simplyblock") {
		t.Fatal("removing the key deleted the ControlPlane")
	}
	if rec := appliedRecord(t, c); len(rec) != 0 {
		t.Fatalf("the removed key is still recorded: %v", rec)
	}
}

func TestUnchangedKeyIsNotAppliedAgain(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(configMap(map[string]string{
		"controlplane.yaml": controlPlaneYAML,
	}, nil)).Build()
	r := &Reconciler{Client: c, Namespace: ns}
	if err := reconcileOnce(t, r); err != nil {
		t.Fatal(err)
	}
	var cp simplyblockv1alpha2.ControlPlane
	if !exists(t, c, &cp, "simplyblock") {
		t.Fatal("not applied")
	}
	// Deleted by hand: an operator restart, or any other reconcile, must not
	// bring a ControlPlane back. Only a changed key does.
	if err := c.Delete(context.Background(), &cp); err != nil {
		t.Fatal(err)
	}
	if err := reconcileOnce(t, r); err != nil {
		t.Fatal(err)
	}
	if exists(t, c, &simplyblockv1alpha2.ControlPlane{}, "simplyblock") {
		t.Fatal("an unchanged key recreated a deleted ControlPlane")
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["controlplane.yaml"] = controlPlaneYAML + "      storageNodeImage: \"repo/image:tag\"\n"
	if err := c.Update(context.Background(), &cm); err != nil {
		t.Fatal(err)
	}
	if err := reconcileOnce(t, r); err != nil {
		t.Fatal(err)
	}
	if !exists(t, c, &cp, "simplyblock") || cp.Spec.Source.Managed.StorageNodeImage != "repo/image:tag" {
		t.Fatal("a changed key was not applied")
	}
}

func TestUpgradeFromAChartThatRenderedTheObjects(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(configMap(map[string]string{
		"controlplane.yaml":      controlPlaneYAML,
		"initial-discovery.yaml": discoveryYAML,
	}, map[string]string{ReleaseIsInstallAnnotation: "false"})).Build()
	r := &Reconciler{Client: c, Namespace: ns}
	if err := reconcileOnce(t, r); err != nil {
		t.Fatal(err)
	}
	if exists(t, c, &simplyblockv1alpha2.OperatorOps{}, "initial-discovery") {
		t.Fatal("the upgrade recreated the discovery run Helm deleted")
	}
	if !exists(t, c, &simplyblockv1alpha2.ControlPlane{}, "simplyblock") {
		t.Fatal("the ControlPlane was not applied on the upgrade")
	}
	if rec := appliedRecord(t, c); rec["initial-discovery.yaml"] != digestOf(discoveryYAML) {
		t.Fatalf("the skipped run should be recorded as settled: %v", rec)
	}
}

func TestRefusesKindsOutsideTheAllowList(t *testing.T) {
	secret := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: x\n"
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(configMap(map[string]string{
		"secret.yaml": secret,
	}, nil)).Build()
	r := &Reconciler{Client: c, Namespace: ns}
	if err := reconcileOnce(t, r); err == nil {
		t.Fatal("a Secret in the ConfigMap was accepted")
	}
	if exists(t, c, &corev1.Secret{}, "x") {
		t.Fatal("the Secret was applied")
	}
}

func TestEmptyKeyIsSettled(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(configMap(map[string]string{
		"controlplane.yaml": "# nothing rendered\n",
	}, nil)).Build()
	r := &Reconciler{Client: c, Namespace: ns}
	if err := reconcileOnce(t, r); err != nil {
		t.Fatal(err)
	}
	if _, ok := appliedRecord(t, c)["controlplane.yaml"]; !ok {
		t.Fatal("an empty key was not recorded")
	}
}
