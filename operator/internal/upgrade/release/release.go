// Reading the deployed Helm release, which §12 differences the new chart
// against.
//
// Helm's Go SDK is not needed to read one. A release is a Secret of type
// helm.sh/release.v1 whose data holds base64 over gzip over JSON, and the JSON
// carries the rendered manifest of everything the release installed. That is
// the whole of what the handover has to enumerate, and decoding it takes the
// standard library.
//
// The SDK is still what renders the *new* chart, which is the other half of
// §12.4's difference. Reading what is deployed and rendering what would replace
// it are separate problems, and only the second one needs Helm.

package release

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// SecretType is what Helm 3 marks its release Secrets with.
const SecretType = "helm.sh/release.v1"

// The labels Helm puts on a release Secret. A release has one Secret per
// revision, and only one of them is the deployed revision.
const (
	labelOwner  = "owner"
	labelStatus = "status"

	ownerHelm      = "helm"
	statusDeployed = "deployed"
)

// Release is a deployed Helm release and what it installed.
type Release struct {
	// Name is the release's name.
	Name string

	// Namespace is where it was installed.
	Namespace string

	// Version is the revision number this is.
	Version int

	// Objects are what the release's manifest declares, in the order the
	// manifest lists them.
	Objects []ObjectRef

	// Values are the values the release was installed with, which §13.1
	// translates into the new chart's spellings before upgrading.
	//
	// They come out of the same JSON as the manifest, so reading them needs no
	// Helm either. What §13.1 wants is the values the user supplied rather
	// than the chart's defaults merged over them, which is what Helm records
	// here.
	Values map[string]any
}

// ObjectRef names one object a release manifest declares.
type ObjectRef struct {
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string
}

// Kind is the object's kind, which is what a reader groups by.
func (r ObjectRef) Kind() string {
	return r.GVK.Kind
}

// String renders the reference the way a plan names it.
func (r ObjectRef) String() string {
	if r.Namespace == "" {
		return fmt.Sprintf("%s %s", r.Kind(), r.Name)
	}
	return fmt.Sprintf("%s %s/%s", r.Kind(), r.Namespace, r.Name)
}

// Deployed returns the deployed release in this namespace, and reports whether
// one is there.
//
// An OLM-installed cluster has none, which is not an error: §13.2 says the
// upgrade takes a different shape there, and the absence is how it is told.
func Deployed(ctx context.Context, c client.Client, namespace string) (*Release, bool, error) {
	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets,
		client.InNamespace(namespace),
		client.MatchingLabels{labelOwner: ownerHelm, labelStatus: statusDeployed},
	); err != nil {
		return nil, false, fmt.Errorf("listing Helm release secrets: %w", err)
	}

	// Highest revision wins. A release keeps its history, and only one Secret
	// carries the deployed label, but a rollback can leave two briefly.
	var newest *Release
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if secret.Type != SecretType {
			continue
		}

		found, err := decode(secret)
		if err != nil {
			return nil, false, fmt.Errorf("reading %s: %w", secret.Name, err)
		}
		if newest == nil || found.Version > newest.Version {
			newest = found
		}
	}
	return newest, newest != nil, nil
}

// stored is the part of Helm's release JSON this reads. The rest of it is the
// chart, the values, and the hooks, none of which the handover enumerates.
type stored struct {
	Name      string         `json:"name"`
	Namespace string         `json:"namespace"`
	Version   int            `json:"version"`
	Manifest  string         `json:"manifest"`
	Config    map[string]any `json:"config"`
}

// decode unwraps one release Secret.
func decode(secret *corev1.Secret) (*Release, error) {
	raw, held := secret.Data["release"]
	if !held {
		return nil, fmt.Errorf("it carries no release data")
	}

	// base64 over gzip over JSON. client-go has already undone the Secret's
	// own encoding, so what is left is Helm's.
	decoded, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		return nil, fmt.Errorf("its release data is not base64: %w", err)
	}
	if decoded, err = gunzip(decoded); err != nil {
		return nil, err
	}

	var held2 stored
	if err := json.Unmarshal(decoded, &held2); err != nil {
		return nil, fmt.Errorf("its release data is not JSON: %w", err)
	}

	objects, err := Objects(held2.Manifest)
	if err != nil {
		return nil, err
	}
	return &Release{
		Name:      held2.Name,
		Namespace: held2.Namespace,
		Version:   held2.Version,
		Objects:   objects,
		Values:    held2.Config,
	}, nil
}

// gunzip decompresses what Helm compressed, passing through a release old
// enough to have been stored uncompressed.
func gunzip(data []byte) ([]byte, error) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, nil
	}

	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("its release data is not gzip: %w", err)
	}
	defer reader.Close() //nolint:errcheck // a read-only reader has nothing to fail on close

	out, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("decompressing its release data: %w", err)
	}
	return out, nil
}

// Objects parses a rendered manifest into the objects it declares.
//
// A document that names no kind is skipped rather than refused. Helm's manifest
// carries the separators and comments of every template that produced it, and a
// template whose whole body is behind a disabled condition renders to nothing.
func Objects(manifest string) ([]ObjectRef, error) {
	var out []ObjectRef

	for _, document := range strings.Split(manifest, "\n---") {
		if strings.TrimSpace(document) == "" {
			continue
		}

		var header struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(document), &header); err != nil {
			// One unreadable document must not lose the other hundred, and a
			// manifest Helm accepted is one the API server took.
			continue
		}
		if header.Kind == "" || header.Metadata.Name == "" {
			continue
		}

		gv, err := schema.ParseGroupVersion(header.APIVersion)
		if err != nil {
			continue
		}
		out = append(out, ObjectRef{
			GVK:       gv.WithKind(header.Kind),
			Namespace: header.Metadata.Namespace,
			Name:      header.Metadata.Name,
		})
	}
	return out, nil
}

// Sorted returns the release's objects in a stable order, which is what a plan
// prints. The manifest's own order is Helm's template order, and it moves when
// a template is added.
func (r *Release) Sorted() []ObjectRef {
	out := append([]ObjectRef(nil), r.Objects...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].GVK.Kind != out[j].GVK.Kind {
			return out[i].GVK.Kind < out[j].GVK.Kind
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}
