// The Service the plugins dial, in the manifests an OLM bundle is built from.
//
// The chart renders this Service. The Kustomize configuration the bundle comes
// from rendered nothing, so an installation through the OpenShift catalog had an
// operator listening on a port that no plugin could reach, and both plugins
// retrying a name that did not resolve. These cases read the files the bundle is
// generated from, because the Service's name is a constant of this package and
// Kustomize's name prefix would otherwise rename it without anything noticing.

package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// readManifests returns every document of a file under operator/config.
func readManifests(t *testing.T, relative string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", relative))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	var documents []map[string]any
	for _, part := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		var document map[string]any
		if err := yaml.Unmarshal([]byte(part), &document); err != nil {
			t.Fatalf("parse %s: %v", relative, err)
		}
		if document != nil {
			documents = append(documents, document)
		}
	}
	return documents
}

func nested(t *testing.T, document map[string]any, path ...string) any {
	t.Helper()
	var current any = document
	for _, key := range path {
		next, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object", strings.Join(path, "."))
		}
		current = next[key]
	}
	return current
}

// The Service carries the name and port the plugins dial.
func TestTheBundleCarriesTheServiceThePluginsDial(t *testing.T) {
	documents := readManifests(t, "csilink/service.yaml")
	if len(documents) != 1 {
		t.Fatalf("csilink/service.yaml holds %d documents, want the one Service", len(documents))
	}
	service := documents[0]

	if kind := nested(t, service, "kind"); kind != "Service" {
		t.Errorf("kind = %v, want Service", kind)
	}
	if name := nested(t, service, "metadata", "name"); name != linkServiceName {
		t.Errorf("name = %v, want %q: the plugins resolve it by name", name, linkServiceName)
	}
	ports, _ := nested(t, service, "spec", "ports").([]any)
	if len(ports) != 1 {
		t.Fatalf("%d ports, want the link's one", len(ports))
	}
	port, _ := ports[0].(map[string]any)
	if got := int(port["port"].(float64)); got != linkPort {
		t.Errorf("port = %d, want %d", got, linkPort)
	}
	if got := int(port["targetPort"].(float64)); got != linkPort {
		t.Errorf("targetPort = %d, want the operator's default listen port %d", got, linkPort)
	}
}

// The Service selects the operator's pods. A selector that matches none is a
// Service with no endpoints, which is the failure this exists to remove.
func TestTheServiceSelectsTheOperatorPods(t *testing.T) {
	selector, _ := nested(t, readManifests(t, "csilink/service.yaml")[0],
		"spec", "selector").(map[string]any)
	if len(selector) == 0 {
		t.Fatal("the Service selects every pod in the namespace")
	}

	var podLabels map[string]any
	for _, document := range readManifests(t, "manager/manager.yaml") {
		if document["kind"] == "Deployment" {
			podLabels, _ = nested(t, document, "spec", "template", "metadata", "labels").(map[string]any)
		}
	}
	if podLabels == nil {
		t.Fatal("the manager Deployment carries no pod labels")
	}
	for key, want := range selector {
		if podLabels[key] != want {
			t.Errorf("selector %s=%v matches no operator pod: its labels are %v", key, want, podLabels)
		}
	}
}

// The bundle is generated from config/manifests, so a Service nobody lists there
// is one the bundle does not have.
func TestTheBundleKustomizationIncludesTheService(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "manifests", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("read the bundle kustomization: %v", err)
	}
	if !strings.Contains(string(raw), "../csilink") {
		t.Error("config/manifests/kustomization.yaml does not list ../csilink")
	}
}
