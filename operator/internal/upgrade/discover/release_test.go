// Tests for reading the deployed release's objects into the graph.
//
// The case that mattered on a real cluster is the one a manifest does not say:
// Helm renders a namespaced object without a namespace and lets it inherit the
// release's, so a reference built straight from the manifest names a ConfigMap
// with no namespace and the API server refuses to look it up.

package discover

import (
	"strings"
	"testing"

	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/release"
)

// releaseManifest holds the three shapes that matter: an object that names its
// namespace, one that does not and inherits the release's, and a cluster-scoped
// one that has none to inherit.
const releaseManifest = `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: simplyblock-config
  namespace: simplyblock
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: simplyblock-caching-node-restart-script-cm
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: local-hostpath
`

// theReleaseNamespace is where the release under test was installed, and what
// an object of it that names no namespace inherits.
const theReleaseNamespace = "simplyblock"

// releaseSecret encodes a manifest the way Helm stores one.
func releaseSecret(t *testing.T) *corev1.Secret {
	t.Helper()

	namespace := theReleaseNamespace

	payload, err := json.Marshal(map[string]any{
		"name": "simplyblock-operator", "namespace": namespace,
		"version": 1, "manifest": releaseManifest,
	})
	if err != nil {
		t.Fatalf("encoding the release: %v", err)
	}

	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("compressing: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the compressor: %v", err)
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sh.helm.release.v1.simplyblock-operator.v1", Namespace: namespace,
			Labels: map[string]string{"owner": "helm", "status": "deployed"},
		},
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))},
	}
}

// mapperFor answers what the API server would about each kind's scope. The fake
// client's default mapper knows nothing, so a test that did not seed one would
// exercise the no-match path for everything and prove nothing.
func mapperFor() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"},
		meta.RESTScopeRoot)
	return mapper
}

// releaseScope builds a scope over a cluster holding the release and these
// objects.
func releaseScope(t *testing.T, objects ...client.Object) *upgrade.Scope {
	t.Helper()

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRESTMapper(mapperFor()).
		WithObjects(objects...).
		Build()
	return upgrade.NewScope(c, "simplyblock", upgrade.StagePreflight,
		upgrade.Options{}, logf.Log, upgrade.DiscardReporter{})
}

func configMapNamed(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: theReleaseNamespace}}
}

func TestHelmRelease_ReadsAnObjectThatNamesItsNamespace(t *testing.T) {
	scope := releaseScope(t,
		releaseSecret(t),
		configMapNamed("simplyblock-config"),
	)

	if err := (helmRelease{}).Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	id := upgrade.ObjectIdentity{Kind: "ConfigMap", Namespace: "simplyblock", Name: "simplyblock-config"}
	if _, held := scope.Graph.Get(id); !held {
		t.Fatal("the ConfigMap the manifest placed was not read")
	}
}

func TestHelmRelease_AnUnqualifiedObjectInheritsTheReleaseNamespace(t *testing.T) {
	// The failure on a real cluster: Helm renders a namespaced object without
	// a namespace, and the API server refuses a lookup that names one and
	// leaves the namespace empty.
	scope := releaseScope(t,
		releaseSecret(t),
		configMapNamed("simplyblock-caching-node-restart-script-cm"),
	)

	if err := (helmRelease{}).Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	id := upgrade.ObjectIdentity{Kind: "ConfigMap", Namespace: "simplyblock", Name: "simplyblock-caching-node-restart-script-cm"}
	if _, held := scope.Graph.Get(id); !held {
		t.Fatal("the ConfigMap that inherits the release's namespace was not read")
	}
}

func TestAddress_ResolvesEachShape(t *testing.T) {
	// Asserted against address rather than through a Get, because the fake
	// client tolerates a namespace on a cluster-scoped lookup where a real API
	// server refuses it. Going through Get would pass whether or not the
	// cluster-scoped branch exists, which is the branch this is here for.
	scope := releaseScope(t)

	for _, tc := range []struct {
		name string
		ref  release.ObjectRef
		want types.NamespacedName
	}{
		{
			name: "an object that names its namespace keeps it",
			ref: release.ObjectRef{
				GVK:       schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
				Namespace: "elsewhere", Name: "config",
			},
			want: types.NamespacedName{Namespace: "elsewhere", Name: "config"},
		},
		{
			name: "an unqualified namespaced object inherits the release's",
			ref: release.ObjectRef{
				GVK:  schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
				Name: "simplyblock-caching-node-restart-script-cm",
			},
			want: types.NamespacedName{Namespace: "simplyblock", Name: "simplyblock-caching-node-restart-script-cm"},
		},
		{
			name: "a cluster-scoped object is given none",
			ref: release.ObjectRef{
				GVK:  schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"},
				Name: "local-hostpath",
			},
			want: types.NamespacedName{Name: "local-hostpath"},
		},
		{
			name: "a namespace on a cluster-scoped object is dropped",
			ref: release.ObjectRef{
				GVK:       schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"},
				Namespace: "simplyblock", Name: "local-hostpath",
			},
			want: types.NamespacedName{Name: "local-hostpath"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, addressable, err := address(scope, tc.ref, "simplyblock")
			if err != nil {
				t.Fatalf("address: %v", err)
			}
			if !addressable {
				t.Fatal("the kind was reported as one the cluster does not serve")
			}
			if got != tc.want {
				t.Fatalf("address = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAddress_ReportsAKindTheClusterDoesNotServe(t *testing.T) {
	scope := releaseScope(t)

	_, addressable, err := address(scope, release.ObjectRef{
		GVK:  schema.GroupVersionKind{Group: "acme.example", Version: "v1", Kind: "Widget"},
		Name: "w",
	}, "simplyblock")
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	if addressable {
		t.Fatal("a kind no mapper knows was reported as addressable")
	}
}

func TestHelmRelease_AnObjectTheClusterNoLongerHoldsIsSkipped(t *testing.T) {
	// Deleted out of band. The handover has nothing to annotate, and failing
	// the run over it would block an upgrade on an object nobody wants.
	scope := releaseScope(t, releaseSecret(t))

	if err := (helmRelease{}).Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if scope.Graph.Len() != 0 {
		t.Fatalf("the graph holds %d objects, and the cluster holds none of the release",
			scope.Graph.Len())
	}
}

func TestHelmRelease_AKindTheClusterDoesNotServeIsSkipped(t *testing.T) {
	// A chart that installed something whose CRD has since gone. There is
	// nothing to hand over, and nothing to read it as.
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRESTMapper(meta.NewDefaultRESTMapper(nil)).
		WithObjects(releaseSecret(t)).
		Build()
	scope := upgrade.NewScope(c, "simplyblock", upgrade.StagePreflight,
		upgrade.Options{}, logf.Log, upgrade.DiscardReporter{})

	if err := (helmRelease{}).Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover reported %v, want nil: an unserved kind is not a failed run", err)
	}
}

func TestHelmRelease_NoReleaseIsNotAFailure(t *testing.T) {
	// §13.2's OLM-installed cluster, where the release does not exist.
	scope := releaseScope(t)

	if err := (helmRelease{}).Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover: %v", err)
	}
}

func TestHelmRelease_ReadsEveryShapeAtOnce(t *testing.T) {
	scope := releaseScope(t,
		releaseSecret(t),
		configMapNamed("simplyblock-config"),
		configMapNamed("simplyblock-caching-node-restart-script-cm"),
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local-hostpath"}},
	)

	if err := (helmRelease{}).Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := scope.Graph.Len(); got != 3 {
		t.Fatalf("the graph holds %d of the release's objects, want all 3", got)
	}
}

func TestHelmRelease_TheDescriptionSaysWhatItIsFor(t *testing.T) {
	if !strings.Contains((helmRelease{}).Description(), "§12") {
		t.Errorf("the discoverer does not cite the section it serves: %q",
			(helmRelease{}).Description())
	}
}
