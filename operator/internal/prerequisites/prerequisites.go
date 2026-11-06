// Package prerequisites detects and installs the cluster add-ons a control plane
// depends on: the FoundationDB CRDs, cert-manager, local-path-provisioner, and
// the CSI snapshot controller. It lives apart from the controllers because it
// is about the cluster, not about any one custom resource.
//
// Each add-on is detected first and installed only when missing. What is
// installed carries LabelInstalledBy and no owner reference, because other
// workloads come to depend on it and it must outlive the ControlPlane.
package prerequisites

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"sort"

	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed manifests/*.yaml
var manifests embed.FS

const (
	// LabelInstalledBy marks an object the operator installed as a
	// prerequisite. Its value is the prerequisite's name.
	LabelInstalledBy = "storage.simplyblock.io/installed-prerequisite"

	fieldOwner = client.FieldOwner("simplyblock-operator-prerequisites")

	kindNamespace  = "Namespace"
	kindCRD        = "CustomResourceDefinition"
	kindDeployment = "Deployment"

	localPathProvisionerName = "rancher.io/local-path"
	defaultClassAnnotation   = "storageclass.kubernetes.io/is-default-class"
	annotationTrue           = "true"
)

// Prerequisite is one add-on: how to tell it is there, and what to apply when
// it is not.
type Prerequisite struct {
	// Name is what events, status messages, and LabelInstalledBy call it.
	Name string

	// probe is a kind whose being served means the add-on is installed. It is
	// empty for an add-on detected some other way.
	probe schema.GroupVersionKind
	// present overrides the probe for an add-on that serves no API.
	present func(context.Context, client.Reader) (bool, error)
	// presentProbe says what present looks for, for the message naming what
	// is missing.
	presentProbe string
	// mutate adjusts the parsed objects against the live cluster before apply.
	mutate func(context.Context, client.Reader, []*unstructured.Unstructured) error
	// marker is the one object whose labels say who installed the add-on and
	// from which manifest digest.
	marker objectRef
	// workload is the Deployment the add-on runs as, nil for CRDs alone.
	workload *objectRef

	// relocatable installs into InstallOptions.Namespace rather than its own.
	relocatable bool

	// AutoUpgrade re-applies an outdated install of ours. Off for an add-on
	// whose upgrades have their own supported path, which is left to a person.
	AutoUpgrade bool

	// awsRoleServiceAccount is the ServiceAccount annotated with
	// InstallOptions.AWSRoleARN, for an add-on that calls AWS APIs.
	awsRoleServiceAccount string

	file string
}

var (
	FoundationDBCRDs = Prerequisite{
		Name:        "foundationdb-crds",
		probe:       schema.GroupVersionKind{Group: "apps.foundationdb.org", Version: "v1beta2", Kind: "FoundationDBCluster"},
		marker:      crdRef("foundationdbclusters.apps.foundationdb.org"),
		AutoUpgrade: true,
		file:        "manifests/foundationdb-crds.yaml",
	}
	CertManager = Prerequisite{
		Name:  "cert-manager",
		probe: schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"},
		// cert-manager supports upgrading one minor version at a time, so an
		// outdated install is reported rather than re-applied.
		marker:   crdRef("certificates.cert-manager.io"),
		workload: deploymentRef("cert-manager", "cert-manager"),
		file:     "manifests/cert-manager.yaml",
	}
	SnapshotController = Prerequisite{
		Name:        "snapshot-controller",
		probe:       schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"},
		marker:      crdRef("volumesnapshots.snapshot.storage.k8s.io"),
		workload:    deploymentRef("kube-system", "simplyblock-snapshot-controller"),
		AutoUpgrade: true,
		file:        "manifests/snapshot-controller.yaml",
	}
	// EBSCSIDriver is the AWS EBS CSI driver, wanted on Amazon EKS only. Any
	// CSIDriver ebs.csi.aws.com counts as present, the EKS add-on's included,
	// and is never touched.
	EBSCSIDriver = Prerequisite{
		Name:                  "aws-ebs-csi-driver",
		present:               ebsCSIDriverPresent,
		presentProbe:          "the CSIDriver " + EBSCSIProvisioner,
		marker:                objectRef{schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "CSIDriver"}, EBSCSIProvisioner, ""},
		workload:              deploymentRef("kube-system", "ebs-csi-controller"),
		AutoUpgrade:           true,
		awsRoleServiceAccount: "ebs-csi-controller-sa",
		file:                  "manifests/aws-ebs-csi-driver.yaml",
	}
	// EBSStorageClass is a gp3 StorageClass of the EBS CSI driver, wanted on
	// Amazon EKS only. A gp3 class of the driver under any name counts as
	// present; GP3StorageClassName says which one the control plane uses.
	EBSStorageClass = Prerequisite{
		Name:         "ebs-gp3-storageclass",
		present:      gp3ClassPresent,
		presentProbe: "a StorageClass using " + EBSCSIProvisioner + " with type gp3",
		mutate:       defaultClassIfNone,
		marker:       objectRef{schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"}, "simplyblock-gp3", ""},
		file:         "manifests/ebs-gp3-storageclass.yaml",
	}
	LocalPathProvisioner = Prerequisite{
		Name:         "local-path-provisioner",
		present:      localPathPresent,
		presentProbe: "a StorageClass using " + localPathProvisionerName,
		mutate:       defaultClassIfNone,
		// Installed beside the control plane, so the SCC and Pod Security
		// grants an OpenShift install already gives that namespace cover it.
		relocatable: true,
		marker:      objectRef{schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"}, "local-path", ""},
		workload:    deploymentRef("local-path-storage", "local-path-provisioner"),
		AutoUpgrade: true,
		file:        "manifests/local-path-provisioner.yaml",
	}
)

// All lists every prerequisite, in the order they are checked.
func All() []Prerequisite {
	return []Prerequisite{FoundationDBCRDs, CertManager, EBSCSIDriver, EBSStorageClass, LocalPathProvisioner, SnapshotController}
}

// Present reports whether the add-on is already on the cluster.
func (p Prerequisite) Present(ctx context.Context, c client.Reader, mapper meta.RESTMapper) (bool, error) {
	if p.present != nil {
		return p.present(ctx, c)
	}
	_, err := mapper.RESTMapping(p.probe.GroupKind(), p.probe.Version)
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	default:
		return false, fmt.Errorf("looking up %s: %w", p.probe, err)
	}
}

// Probe names what Present looks for, for a message that says what is missing.
func (p Prerequisite) Probe() string {
	if p.present != nil {
		return p.presentProbe
	}
	return p.probe.GroupVersion().String()
}

// Objects parses the embedded manifest, labels every object, and orders
// Namespaces and CRDs first so nothing is applied before what it depends on.
func (p Prerequisite) Objects() ([]*unstructured.Unstructured, error) {
	return p.objects(InstallOptions{})
}

// objects is Objects with the images moved to the mirror registry and every
// object annotated with the digest it was built from.
func (p Prerequisite) objects(opts InstallOptions) ([]*unstructured.Unstructured, error) {
	raw, err := manifests.ReadFile(p.file)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p.file, err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var objs []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parsing %s: %w", p.file, err)
		}
		if len(u.Object) == 0 {
			continue
		}
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[LabelInstalledBy] = p.Name
		u.SetLabels(labels)
		annotations := u.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[AnnotationDigest] = p.Digest(opts)
		u.SetAnnotations(annotations)
		rewriteImages(u, opts.ImageRegistry)
		if err := addTolerations(u, opts.Tolerations); err != nil {
			return nil, fmt.Errorf("%s: %w", p.file, err)
		}
		if opts.AWSRoleARN != "" && u.GetKind() == "ServiceAccount" && u.GetName() == p.awsRoleServiceAccount {
			annotations[AnnotationAWSRoleARN] = opts.AWSRoleARN
			u.SetAnnotations(annotations)
		}
		if p.relocatable && opts.Namespace != "" {
			if u.GetKind() == kindNamespace {
				continue
			}
			relocate(u, opts.Namespace)
		}
		objs = append(objs, u)
	}
	sort.SliceStable(objs, func(i, j int) bool { return rank(objs[i]) < rank(objs[j]) })
	return objs, nil
}

// relocate moves a namespaced object, and every ServiceAccount subject a
// binding names, into namespace.
func relocate(u *unstructured.Unstructured, namespace string) {
	if u.GetNamespace() != "" {
		u.SetNamespace(namespace)
	}
	subjects, found, _ := unstructured.NestedSlice(u.Object, "subjects")
	if !found {
		return
	}
	for _, s := range subjects {
		if m, ok := s.(map[string]any); ok && m["kind"] == "ServiceAccount" {
			m["namespace"] = namespace
		}
	}
	_ = unstructured.SetNestedSlice(u.Object, subjects, "subjects")
}

// isMarker reports whether u is the object State reads.
func (p Prerequisite) isMarker(u *unstructured.Unstructured) bool {
	return u.GetKind() == p.marker.gvk.Kind && u.GetName() == p.marker.name
}

func rank(u *unstructured.Unstructured) int {
	switch u.GetKind() {
	case kindNamespace:
		return 0
	case kindCRD:
		return 1
	default:
		return 2
	}
}

// Install server-side applies every object of the manifest. It is idempotent,
// so an install interrupted part way is finished by calling it again, and it is
// also the upgrade of an outdated install. Objects an older manifest had and
// this one dropped are left in place.
func (p Prerequisite) Install(ctx context.Context, c client.Client, opts InstallOptions) error {
	objs, err := p.objects(opts)
	if err != nil {
		return err
	}
	if p.mutate != nil {
		if err := p.mutate(ctx, c, objs); err != nil {
			return err
		}
	}
	// The marker goes last: its digest is what records the install as current,
	// so it changes only once every other object has been applied.
	sort.SliceStable(objs, func(i, j int) bool { return !p.isMarker(objs[i]) && p.isMarker(objs[j]) })
	for _, o := range objs {
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(o), fieldOwner, client.ForceOwnership); err != nil {
			return fmt.Errorf("installing %s: applying %s %s: %w", p.Name, o.GetKind(), o.GetName(), err)
		}
	}
	return nil
}

func localPathPresent(ctx context.Context, c client.Reader) (bool, error) {
	var classes storagev1.StorageClassList
	if err := c.List(ctx, &classes); err != nil {
		return false, fmt.Errorf("listing StorageClasses: %w", err)
	}
	for _, sc := range classes.Items {
		if sc.Provisioner == localPathProvisionerName {
			return true, nil
		}
	}
	return false, nil
}

// defaultClassIfNone marks the installed class as the default on a
// cluster that has none, and never displaces an existing default.
func defaultClassIfNone(ctx context.Context, c client.Reader, objs []*unstructured.Unstructured) error {
	var classes storagev1.StorageClassList
	if err := c.List(ctx, &classes); err != nil {
		return fmt.Errorf("listing StorageClasses: %w", err)
	}
	for _, sc := range classes.Items {
		if sc.Annotations[defaultClassAnnotation] == annotationTrue {
			return nil
		}
	}
	for _, o := range objs {
		if o.GetKind() != "StorageClass" {
			continue
		}
		a := o.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a[defaultClassAnnotation] = annotationTrue
		o.SetAnnotations(a)
	}
	return nil
}
