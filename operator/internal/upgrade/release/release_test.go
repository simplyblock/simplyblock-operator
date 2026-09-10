// Tests for reading a deployed Helm release. What they hold still is the
// encoding, because it is Helm's and not ours: a release is base64 over gzip
// over JSON, and a decoder that guessed one layer wrong would report an
// installation as having nothing in it.

package release

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const manifest = `---
# Source: simplyblock-operator/templates/controlplane.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: simplyblock-webappapi
  namespace: simplyblock
---
# Source: simplyblock-operator/templates/csi-node.yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: simplyblock-csi-node
  namespace: simplyblock
---
apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata:
  name: csi.simplyblock.io
---
# a template whose body is behind a disabled condition renders to nothing
---
`

// secretFor encodes a release the way Helm stores one.
func secretFor(t *testing.T, name string, version int, compressed bool) *corev1.Secret {
	t.Helper()

	payload, err := json.Marshal(stored{
		Name: name, Namespace: "simplyblock", Version: version, Manifest: manifest,
	})
	if err != nil {
		t.Fatalf("encoding the release: %v", err)
	}

	if compressed {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(payload); err != nil {
			t.Fatalf("compressing: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("closing the compressor: %v", err)
		}
		payload = buf.Bytes()
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1." + name + ".v" + string(rune('0'+version)),
			Namespace: "simplyblock",
			Labels:    map[string]string{labelOwner: ownerHelm, labelStatus: statusDeployed},
		},
		Type: SecretType,
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(payload))},
	}
}

func clusterWith(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestDeployed_ReadsTheReleaseHelmStored(t *testing.T) {
	deployed, found, err := Deployed(t.Context(), clusterWith(t, secretFor(t, "simplyblock-operator", 1, true)), "simplyblock")
	if err != nil || !found {
		t.Fatalf("Deployed: %v, found=%v", err, found)
	}

	if deployed.Name != "simplyblock-operator" {
		t.Errorf("name = %q", deployed.Name)
	}
	if len(deployed.Objects) != 3 {
		t.Fatalf("read %d objects, want the three the manifest declares:\n%v",
			len(deployed.Objects), deployed.Objects)
	}
}

func TestDeployed_ReadsAReleaseStoredUncompressed(t *testing.T) {
	// Helm gzips, and a release old enough not to have been is still one this
	// has to read rather than refuse.
	deployed, found, err := Deployed(t.Context(), clusterWith(t, secretFor(t, "old", 1, false)), "simplyblock")
	if err != nil || !found {
		t.Fatalf("Deployed: %v, found=%v", err, found)
	}
	if len(deployed.Objects) != 3 {
		t.Fatalf("read %d objects from an uncompressed release", len(deployed.Objects))
	}
}

func TestDeployed_TakesTheHighestRevision(t *testing.T) {
	c := clusterWith(t, secretFor(t, "rel", 1, true), secretFor(t, "rel", 3, true), secretFor(t, "rel", 2, true))

	deployed, found, err := Deployed(t.Context(), c, "simplyblock")
	if err != nil || !found {
		t.Fatalf("Deployed: %v, found=%v", err, found)
	}
	if deployed.Version != 3 {
		t.Fatalf("version = %d, want the highest revision", deployed.Version)
	}
}

func TestDeployed_NoReleaseIsNotAnError(t *testing.T) {
	// §13.2's OLM-installed cluster has none, and the absence is how the two
	// installation channels are told apart.
	_, found, err := Deployed(t.Context(), clusterWith(t), "simplyblock")
	if err != nil {
		t.Fatalf("Deployed: %v", err)
	}
	if found {
		t.Fatal("a cluster with no Helm release reported one")
	}
}

func TestDeployed_IgnoresASecretThatIsNotARelease(t *testing.T) {
	other := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "something", Namespace: "simplyblock",
			Labels: map[string]string{labelOwner: ownerHelm, labelStatus: statusDeployed},
		},
		Type: corev1.SecretTypeOpaque,
	}

	_, found, err := Deployed(t.Context(), clusterWith(t, other), "simplyblock")
	if err != nil {
		t.Fatalf("Deployed: %v", err)
	}
	if found {
		t.Fatal("an Opaque secret carrying Helm's labels was read as a release")
	}
}

func TestObjects_SkipsADocumentThatDeclaresNothing(t *testing.T) {
	// A manifest carries the separators and comments of every template that
	// produced it, and a template behind a disabled condition renders to
	// nothing.
	objects, err := Objects(manifest)
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}
	for _, ref := range objects {
		if ref.Kind() == "" || ref.Name == "" {
			t.Errorf("read an object with no kind or name: %+v", ref)
		}
	}
}

func TestObjects_ReadsTheGroupAndTheKind(t *testing.T) {
	objects, err := Objects(manifest)
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}

	var driver *ObjectRef
	for i := range objects {
		if objects[i].Kind() == "CSIDriver" {
			driver = &objects[i]
		}
	}
	if driver == nil {
		t.Fatalf("the CSIDriver was not read:\n%v", objects)
	}
	if driver.GVK.Group != "storage.k8s.io" {
		t.Errorf("group = %q, want the one its apiVersion names", driver.GVK.Group)
	}
	// Cluster-scoped, so it carries no namespace, and asking Helm's manifest
	// for one would invent it.
	if driver.Namespace != "" {
		t.Errorf("namespace = %q on a cluster-scoped object", driver.Namespace)
	}
}

func TestSorted_IsStable(t *testing.T) {
	// The manifest's own order is Helm's template order and moves when a
	// template is added, so a plan built on it would reorder itself.
	deployed, _, err := Deployed(t.Context(), clusterWith(t, secretFor(t, "rel", 1, true)), "simplyblock")
	if err != nil {
		t.Fatalf("Deployed: %v", err)
	}

	first := render(deployed.Sorted())
	for range 10 {
		if got := render(deployed.Sorted()); got != first {
			t.Fatalf("two sorts differ:\n%s\n%s", first, got)
		}
	}
	if !strings.Contains(first, "CSIDriver") {
		t.Errorf("the sorted list lost an object:\n%s", first)
	}
}

func render(refs []ObjectRef) string {
	var b strings.Builder
	for _, ref := range refs {
		b.WriteString(ref.String())
		b.WriteString("\n")
	}
	return b.String()
}

func TestDeployed_ReadsTheValuesTheReleaseWasInstalledWith(t *testing.T) {
	// §13.1 translates them into the new chart's spellings before upgrading,
	// and they sit in the same JSON as the manifest, so reading them needs no
	// Helm either.
	secret := secretFor(t, "simplyblock-operator", 1, true)

	payload, err := json.Marshal(map[string]any{
		"name": "simplyblock-operator", "namespace": "simplyblock", "version": 1,
		"manifest": manifest,
		"config": map[string]any{
			"storagenode":  map[string]any{"skipKubeletConfiguration": true},
			"multiCluster": map[string]any{"enable": false},
		},
	})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("compressing: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	secret.Data["release"] = []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))

	deployed, found, err := Deployed(t.Context(), clusterWith(t, secret), "simplyblock")
	if err != nil || !found {
		t.Fatalf("Deployed: %v, found=%v", err, found)
	}

	node, ok := deployed.Values["storagenode"].(map[string]any)
	if !ok {
		t.Fatalf("values = %v, want the ones the release carries", deployed.Values)
	}
	// The key §13.1 renames, which is what makes the translation necessary.
	if node["skipKubeletConfiguration"] != true {
		t.Errorf("the deployed value was not read: %v", node)
	}
}
