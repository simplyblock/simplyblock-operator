// Tests for the control plane's volumes on Amazon EKS: before the database
// exists, the operator brings the EBS CSI driver and a gp3 class of it and
// records the class; a stated class, another distribution, or a database that
// exists already keeps the behaviour from before.

package controlplane

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/prerequisites"
)

func eksNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{"eks.amazonaws.com/nodegroup": "system"},
	}}
}

func plainNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func wantedNames(cp *simplyblockv1alpha2.ControlPlane, chooseEBSClass bool) []string {
	var names []string
	for _, w := range wantedPrerequisites(cp, chooseEBSClass) {
		names = append(names, w.Name)
	}
	return names
}

func chooses(t *testing.T, cp *simplyblockv1alpha2.ControlPlane, objs ...client.Object) bool {
	t.Helper()
	c := newClient(t, append([]client.Object{cp}, objs...)...)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}
	got, err := r.choosesEBSClass(context.Background(), cp)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// On EKS the driver and the class replace local-path; elsewhere nothing changes.
func TestEKSWantsTheEBSDriverAndAGP3ClassInsteadOfLocalPath(t *testing.T) {
	cp := localControlPlane()

	eks := wantedNames(cp, true)
	for _, want := range []string{prerequisites.EBSCSIDriver.Name, prerequisites.EBSStorageClass.Name} {
		if !slices.Contains(eks, want) {
			t.Errorf("on EKS, %s is not wanted: %v", want, eks)
		}
	}
	if slices.Contains(eks, prerequisites.LocalPathProvisioner.Name) {
		t.Errorf("on EKS, local-path is still wanted: %v", eks)
	}

	other := wantedNames(cp, false)
	if !slices.Contains(other, prerequisites.LocalPathProvisioner.Name) || slices.Contains(other, prerequisites.EBSCSIDriver.Name) {
		t.Errorf("off EKS, wanted = %v, want local-path and no EBS driver", other)
	}
}

func TestTheEBSClassIsChosenOnlyOnEKSBeforeTheDatabaseExists(t *testing.T) {
	database := &unstructured.Unstructured{}
	database.SetGroupVersionKind(fdbClusterGVK)
	database.SetNamespace(testNamespace)
	database.SetName(ComponentFDBCluster)

	stated := localControlPlane()
	stated.Spec.Source.Local.FoundationDB = &simplyblockv1alpha2.FoundationDBSpec{StorageClassName: "fast"}
	chosen := localControlPlane()
	chosen.Status.StorageClassName = "ebs-gp3"

	for _, tc := range []struct {
		name string
		cp   *simplyblockv1alpha2.ControlPlane
		objs []client.Object
		want bool
	}{
		{"a fresh EKS cluster", localControlPlane(), []client.Object{eksNode("a"), eksNode("b")}, true},
		{"another distribution", localControlPlane(), []client.Object{plainNode("a")}, false},
		{"a class stated in the spec", stated, []client.Object{eksNode("a")}, false},
		{"a class chosen before", chosen, []client.Object{eksNode("a")}, false},
		{"a database that exists already", localControlPlane(), []client.Object{eksNode("a"), database}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooses(t, tc.cp, tc.objs...); got != tc.want {
				t.Errorf("choosesEBSClass = %v, want %v", got, tc.want)
			}
		})
	}
}

// A gp3 class that exists under another name is used: nothing is created, the
// class is recorded, and the database's volumes are claimed from it.
func TestAnExistingGP3ClassIsRecordedAndUsed(t *testing.T) {
	ctx := context.Background()
	cp := localControlPlane()
	existing := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "team-gp3"},
		Provisioner: prerequisites.EBSCSIProvisioner,
		Parameters:  map[string]string{"type": "gp3"},
	}
	driver := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: prerequisites.EBSCSIProvisioner}}
	c := newClient(t, cp, eksNode("a"), existing, driver)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}); err != nil {
		t.Fatal(err)
	}

	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(ctx, client.ObjectKeyFromObject(cp), &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.StorageClassName != "team-gp3" {
		t.Errorf("status.storageClassName = %q, want team-gp3", after.Status.StorageClassName)
	}
	var ours storagev1.StorageClass
	if err := c.Get(ctx, client.ObjectKey{Name: "simplyblock-gp3"}, &ours); err == nil {
		t.Error("simplyblock-gp3 was created beside an existing gp3 class")
	}
	var localPath storagev1.StorageClass
	if err := c.Get(ctx, client.ObjectKey{Name: "local-path"}, &localPath); err == nil {
		t.Error("local-path was installed on EKS")
	}
	if claim := volumeClaimSpec(storageClassOf(&after)); claim["storageClassName"] != "team-gp3" {
		t.Errorf("the database's claim names %v, want team-gp3", claim["storageClassName"])
	}
}

// On a fresh EKS cluster without a driver, ours and its class are installed,
// and the install holds until the class is recorded on a later pass.
func TestAFreshEKSClusterGetsTheDriverAndTheClass(t *testing.T) {
	ctx := context.Background()
	cp := localControlPlane()
	c := newClient(t, cp, eksNode("a"))
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}); err != nil {
		t.Fatal(err)
	}
	var driver storagev1.CSIDriver
	if err := c.Get(ctx, client.ObjectKey{Name: prerequisites.EBSCSIProvisioner}, &driver); err != nil {
		t.Errorf("the EBS CSI driver was not installed: %v", err)
	}
	var class storagev1.StorageClass
	if err := c.Get(ctx, client.ObjectKey{Name: "simplyblock-gp3"}, &class); err != nil {
		t.Errorf("the gp3 class was not created: %v", err)
	}
	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(ctx, client.ObjectKeyFromObject(cp), &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.StorageClassName != "simplyblock-gp3" {
		t.Errorf("status.storageClassName = %q, want simplyblock-gp3", after.Status.StorageClassName)
	}
}
