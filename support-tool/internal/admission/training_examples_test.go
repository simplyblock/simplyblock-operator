// The example manifests of the operator training, operator/docs/training/examples,
// held to what the API server would admit. A chapter teaches by showing a
// manifest, and one the API refuses teaches the wrong thing; this is the only
// place with the operator's CRDs and the API server's validators in one
// package, so the check lives here rather than beside the pages.
package admission

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

const trainingExamples = "../../../operator/docs/training/examples"

// TestTrainingExamplesAreAdmitted validates every document of every manifest
// under the training examples whose group is the operator's. Other kinds (a
// Secret, a StorageClass, a NodeHealthCheck) are shown beside them and have no
// CRD here, so they are skipped rather than refused.
func TestTrainingExamplesAreAdmitted(t *testing.T) {
	var files []string
	err := filepath.WalkDir(trainingExamples, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", trainingExamples, err)
	}
	if len(files) == 0 {
		t.Fatalf("no manifests under %s: the directory moved, or the chapters have no examples", trainingExamples)
	}
	checked := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, doc := range bytes.Split(raw, []byte("\n---")) {
			if strings.TrimSpace(string(doc)) == "" {
				continue
			}
			var probe map[string]interface{}
			if err := yaml.Unmarshal(doc, &probe); err != nil {
				t.Errorf("%s document %d: %v", path, i+1, err)
				continue
			}
			kind, _ := probe["kind"].(string)
			apiVersion, _ := probe["apiVersion"].(string)
			if kind == "" || !strings.HasPrefix(apiVersion, "storage.simplyblock.io/") {
				// A comment-only document, a Helm values file, or a kind of
				// another group.
				continue
			}
			// The YAML library decodes every number as a float, which the CEL
			// rules refuse for an integer field. Unstructured's own decoder
			// reads integers as int64, the way the API server does.
			jsonDoc, err := yaml.YAMLToJSON(doc)
			if err != nil {
				t.Errorf("%s document %d: %v", path, i+1, err)
				continue
			}
			obj := &unstructured.Unstructured{}
			if err := obj.UnmarshalJSON(jsonDoc); err != nil {
				t.Errorf("%s document %d: %v", path, i+1, err)
				continue
			}
			checked++
			refusals, err := Validate(obj)
			if err != nil {
				t.Errorf("%s document %d (%s %s): %v", path, i+1, obj.GetKind(), obj.GetName(), err)
				continue
			}
			for _, r := range refusals {
				t.Errorf("%s document %d (%s %s): %s", path, i+1, obj.GetKind(), obj.GetName(), r)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("no storage.simplyblock.io manifest under %s", trainingExamples)
	}
}
