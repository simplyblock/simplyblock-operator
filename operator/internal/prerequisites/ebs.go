// What Amazon EKS adds to the prerequisites: the EBS CSI driver and a gp3
// StorageClass of it for the control plane's volumes, and how an add-on is
// placed on a cluster whose nodes are all tainted.

package prerequisites

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// EBSCSIProvisioner is the AWS EBS CSI driver's name, which is both its
	// CSIDriver object and the provisioner of its StorageClasses.
	EBSCSIProvisioner = "ebs.csi.aws.com"

	// AnnotationAWSRoleARN makes a ServiceAccount authenticate through an IAM
	// role on Amazon EKS.
	AnnotationAWSRoleARN = "eks.amazonaws.com/role-arn"

	ebsVolumeTypeParameter = "type"
	ebsGP3                 = "gp3"
)

func ebsCSIDriverPresent(ctx context.Context, c client.Reader) (bool, error) {
	var driver storagev1.CSIDriver
	err := c.Get(ctx, client.ObjectKey{Name: EBSCSIProvisioner}, &driver)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("reading the CSIDriver %s: %w", EBSCSIProvisioner, err)
	}
}

func gp3ClassPresent(ctx context.Context, c client.Reader) (bool, error) {
	name, err := GP3StorageClassName(ctx, c)
	return name != "", err
}

// GP3StorageClassName is the StorageClass the control plane's volumes use on
// Amazon EKS: a class of the EBS CSI driver that provisions gp3 volumes. Of
// several, the cluster's default class wins, then the first by name, so the
// choice is the same on every reconcile. It is empty when there is none.
func GP3StorageClassName(ctx context.Context, c client.Reader) (string, error) {
	var classes storagev1.StorageClassList
	if err := c.List(ctx, &classes); err != nil {
		return "", fmt.Errorf("listing StorageClasses: %w", err)
	}
	var names []string
	for _, sc := range classes.Items {
		if sc.Provisioner != EBSCSIProvisioner || !strings.EqualFold(sc.Parameters[ebsVolumeTypeParameter], ebsGP3) {
			continue
		}
		if sc.Annotations[defaultClassAnnotation] == annotationTrue {
			return sc.Name, nil
		}
		names = append(names, sc.Name)
	}
	if len(names) == 0 {
		return "", nil
	}
	sort.Strings(names)
	return names[0], nil
}

// addTolerations adds tolerations to the pod template of a Deployment or a
// Job, so an add-on schedules where the control plane does on a cluster whose
// nodes are all tainted. A toleration the template has already is not repeated.
// DaemonSets are left alone: the ones the add-ons ship tolerate what they need.
func addTolerations(u *unstructured.Unstructured, tolerations []corev1.Toleration) error {
	if len(tolerations) == 0 || (u.GetKind() != kindDeployment && u.GetKind() != "Job") {
		return nil
	}
	path := []string{"spec", "template", "spec", "tolerations"}
	raw, _, err := unstructured.NestedSlice(u.Object, path...)
	if err != nil {
		return fmt.Errorf("%s %s: reading its tolerations: %w", u.GetKind(), u.GetName(), err)
	}
	var have []corev1.Toleration
	for _, r := range raw {
		var t corev1.Toleration
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &t); err != nil {
			return fmt.Errorf("%s %s: reading its tolerations: %w", u.GetKind(), u.GetName(), err)
		}
		have = append(have, t)
	}
	for _, t := range tolerations {
		if hasToleration(have, t) {
			continue
		}
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&t)
		if err != nil {
			return err
		}
		raw = append(raw, m)
		have = append(have, t)
	}
	return unstructured.SetNestedSlice(u.Object, raw, path...)
}

func hasToleration(have []corev1.Toleration, t corev1.Toleration) bool {
	for _, h := range have {
		if h.MatchToleration(&t) {
			return true
		}
	}
	return false
}
