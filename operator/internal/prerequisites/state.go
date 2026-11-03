// Who installed an add-on and whether it is current: the marker object's
// installed-by label and manifest digest decide whether the operator installs,
// upgrades, or leaves it alone.

package prerequisites

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AnnotationDigest records the manifest digest an installed object was built from.
const AnnotationDigest = "storage.simplyblock.io/prerequisite-digest"

// State is what the cluster has of an add-on.
type State string

const (
	// Absent is nothing installed.
	Absent State = "Absent"
	// Foreign is installed by somebody else, and never touched.
	Foreign State = "Foreign"
	// Current is installed by the operator from the embedded manifest.
	Current State = "Current"
	// Outdated is installed by the operator from an older manifest.
	Outdated State = "Outdated"
)

type objectRef struct {
	gvk       schema.GroupVersionKind
	name      string
	namespace string
}

// deploymentRef names the Deployment an add-on runs as.
func deploymentRef(namespace, name string) *objectRef {
	return &objectRef{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, name, namespace}
}

func crdRef(name string) objectRef {
	return objectRef{schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: kindCRD}, name, ""}
}

// Digest identifies the manifest and the mirror registry it is installed with,
// so a new operator release or a changed registry both read as Outdated.
func (p Prerequisite) Digest(opts InstallOptions) string {
	raw, err := manifests.ReadFile(p.file)
	if err != nil {
		panic(err) // the embedded set is fixed at build time
	}
	h := sha256.New()
	h.Write(raw)
	h.Write([]byte{0})
	h.Write([]byte(opts.ImageRegistry))
	if p.relocatable {
		h.Write([]byte{0})
		h.Write([]byte(opts.Namespace))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// State reads the marker object and reports who installed the add-on.
func (p Prerequisite) State(ctx context.Context, c client.Reader, opts InstallOptions) (State, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(p.marker.gvk)
	err := c.Get(ctx, client.ObjectKey{Name: p.marker.name}, u)
	switch {
	case apierrors.IsNotFound(err):
		// local-path run by somebody else need not use the class name ours does.
		if p.present != nil {
			if found, err := p.present(ctx, c); err != nil || found {
				return Foreign, err
			}
		}
		return Absent, nil
	case err != nil:
		return "", fmt.Errorf("reading %s %s: %w", p.marker.gvk.Kind, p.marker.name, err)
	case u.GetLabels()[LabelInstalledBy] != p.Name:
		return Foreign, nil
	case u.GetAnnotations()[AnnotationDigest] != p.Digest(opts):
		return Outdated, nil
	}
	// The marker can outlive the workload, as local-path's class does its
	// namespace, so a current install whose Deployment is gone reads as Absent.
	return p.workloadState(ctx, c, opts)
}

func (p Prerequisite) workloadState(ctx context.Context, c client.Reader, opts InstallOptions) (State, error) {
	if p.workload == nil {
		return Current, nil
	}
	namespace := p.workload.namespace
	if p.relocatable && opts.Namespace != "" {
		namespace = opts.Namespace
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(p.workload.gvk)
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: p.workload.name}, u)
	switch {
	case apierrors.IsNotFound(err):
		return Absent, nil
	case err != nil:
		return "", fmt.Errorf("reading %s %s/%s: %w", p.workload.gvk.Kind, namespace, p.workload.name, err)
	default:
		return Current, nil
	}
}

// CRDNames lists every CRD the embedded manifests define, which is what the
// operator's named patch grant on CustomResourceDefinitions has to cover.
func CRDNames() []string {
	var names []string
	for _, p := range All() {
		objs, err := p.Objects()
		if err != nil {
			panic(err)
		}
		for _, o := range objs {
			if o.GetKind() == kindCRD {
				names = append(names, o.GetName())
			}
		}
	}
	return names
}
