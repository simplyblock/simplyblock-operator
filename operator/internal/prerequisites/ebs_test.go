// Tests for the Amazon EKS prerequisites: an EBS CSI driver or a gp3 class of
// it that is already there is used and never touched, ours is installed only
// when there is none, and the add-ons schedule on a cluster of tainted nodes.

package prerequisites

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func ebsClass(name, volumeType string, isDefault bool) *storagev1.StorageClass {
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: EBSCSIProvisioner,
		Parameters:  map[string]string{"type": volumeType},
	}
	if isDefault {
		sc.Annotations = map[string]string{defaultClassAnnotation: annotationTrue}
	}
	return sc
}

func clientWith(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
}

var controlPlaneTaint = corev1.Toleration{
	Key: "simplyblock.io/role", Operator: corev1.TolerationOpEqual, Value: "controlplane", Effect: corev1.TaintEffectNoSchedule,
}

// The class is any gp3 class of the driver, whatever its name; the default one
// wins, then the first by name, and a gp2 class or another driver's is no answer.
func TestTheGP3ClassIsFoundUnderAnyName(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		classes []client.Object
		want    string
	}{
		{"none", nil, ""},
		{"only gp2", []client.Object{ebsClass("gp2", "gp2", true)}, ""},
		{"another driver's gp3", []client.Object{&storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: "other"}, Provisioner: "example.com/csi",
			Parameters: map[string]string{"type": "gp3"},
		}}, ""},
		{"one under its own name", []client.Object{ebsClass("ebs-gp3", "gp3", false)}, "ebs-gp3"},
		{"the type in capitals", []client.Object{ebsClass("fast", "GP3", false)}, "fast"},
		{"the first by name", []client.Object{ebsClass("zeta", "gp3", false), ebsClass("alpha", "gp3", false)}, "alpha"},
		{"the default first", []client.Object{ebsClass("alpha", "gp3", false), ebsClass("zeta", "gp3", true)}, "zeta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GP3StorageClassName(ctx, clientWith(tc.classes...))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("GP3StorageClassName = %q, want %q", got, tc.want)
			}
		})
	}
}

// A gp3 class somebody else created is present, and ours is not installed.
func TestAnExistingGP3ClassIsForeign(t *testing.T) {
	ctx := context.Background()
	c := clientWith(ebsClass("ebs-gp3", "gp3", false))
	state, err := EBSStorageClass.State(ctx, c, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if state != Foreign {
		t.Errorf("state = %s, want Foreign", state)
	}
}

// The EKS add-on's driver is present, and ours is not installed over it.
func TestTheEKSAddOnDriverIsForeign(t *testing.T) {
	ctx := context.Background()
	c := clientWith(&storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{Name: EBSCSIProvisioner, Labels: map[string]string{"app.kubernetes.io/managed-by": "EKS"}},
	})
	state, err := EBSCSIDriver.State(ctx, c, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if state != Foreign {
		t.Errorf("state = %s, want Foreign", state)
	}
	present, err := EBSCSIDriver.Present(ctx, c, nil)
	if err != nil || !present {
		t.Errorf("Present = %v, %v; want true", present, err)
	}
}

// Our class becomes the default only on a cluster that has none.
func TestTheGP3ClassIsTheDefaultOnlyWhereThereIsNone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		existing []client.Object
		want     bool
	}{
		{"no default", nil, true},
		{"a default already", []client.Object{ebsClass("gp2", "gp2", true)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clientWith(tc.existing...)
			if err := EBSStorageClass.Install(ctx, c, InstallOptions{}); err != nil {
				t.Fatal(err)
			}
			var sc storagev1.StorageClass
			if err := c.Get(ctx, client.ObjectKey{Name: "simplyblock-gp3"}, &sc); err != nil {
				t.Fatal(err)
			}
			if sc.Provisioner != EBSCSIProvisioner || sc.Parameters["type"] != "gp3" {
				t.Errorf("class = %s %v, want %s with type gp3", sc.Provisioner, sc.Parameters, EBSCSIProvisioner)
			}
			if got := sc.Annotations[defaultClassAnnotation] == annotationTrue; got != tc.want {
				t.Errorf("default = %v, want %v", got, tc.want)
			}
			name, err := GP3StorageClassName(ctx, c)
			if err != nil || name != "simplyblock-gp3" {
				t.Errorf("GP3StorageClassName = %q, %v; want simplyblock-gp3", name, err)
			}
		})
	}
}

// The driver's controller tolerates the control plane's taint and assumes the
// IAM role; the node DaemonSet keeps tolerating every taint on its own.
func TestTheDriverIsPlacedAndAuthenticated(t *testing.T) {
	ctx := context.Background()
	c := clientWith()
	opts := InstallOptions{
		Tolerations: []corev1.Toleration{controlPlaneTaint},
		AWSRoleARN:  "arn:aws:iam::123456789012:role/simplyblock",
	}
	if err := EBSCSIDriver.Install(ctx, c, opts); err != nil {
		t.Fatal(err)
	}
	var controller appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "ebs-csi-controller"}, &controller); err != nil {
		t.Fatal(err)
	}
	if !hasToleration(controller.Spec.Template.Spec.Tolerations, controlPlaneTaint) {
		t.Errorf("the controller does not tolerate the control plane's taint: %v", controller.Spec.Template.Spec.Tolerations)
	}
	var sa corev1.ServiceAccount
	if err := c.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "ebs-csi-controller-sa"}, &sa); err != nil {
		t.Fatal(err)
	}
	if sa.Annotations[AnnotationAWSRoleARN] != opts.AWSRoleARN {
		t.Errorf("controller ServiceAccount annotations = %v, want the role", sa.Annotations)
	}
	var node appsv1.DaemonSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "ebs-csi-node"}, &node); err != nil {
		t.Fatal(err)
	}
	if !hasToleration(node.Spec.Template.Spec.Tolerations, corev1.Toleration{Operator: corev1.TolerationOpExists}) {
		t.Errorf("the node plugin no longer tolerates every taint: %v", node.Spec.Template.Spec.Tolerations)
	}
	state, err := EBSCSIDriver.State(ctx, c, opts)
	if err != nil || state != Current {
		t.Errorf("state after install = %s, %v; want Current", state, err)
	}
}

// cert-manager's Deployments schedule where the control plane does, and a
// toleration a template has already is not added twice.
func TestTolerationsReachEveryDeploymentOnce(t *testing.T) {
	objs, err := CertManager.objects(InstallOptions{Tolerations: []corev1.Toleration{controlPlaneTaint, controlPlaneTaint}})
	if err != nil {
		t.Fatal(err)
	}
	deployments := 0
	for _, o := range objs {
		if o.GetKind() != kindDeployment {
			continue
		}
		deployments++
		raw, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "tolerations")
		count := 0
		for _, r := range raw {
			if m, ok := r.(map[string]any); ok && m["key"] == controlPlaneTaint.Key {
				count++
			}
		}
		if count != 1 {
			t.Errorf("Deployment %s tolerates the control plane's taint %d times, want once", o.GetName(), count)
		}
	}
	if deployments == 0 {
		t.Fatal("cert-manager has no Deployment")
	}
}

// Without tolerations the manifests are applied as they are.
func TestNoTolerationsLeaveTheTemplatesAlone(t *testing.T) {
	with, err := SnapshotController.objects(InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range with {
		if o.GetKind() != kindDeployment {
			continue
		}
		if raw, found, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "tolerations"); found && len(raw) > 0 {
			t.Errorf("Deployment %s gained tolerations: %v", o.GetName(), raw)
		}
	}
}
