package utils

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSamplesKustomizationReferencesExistingFiles(t *testing.T) {
	const kustomizationPath = "../../config/samples/kustomization.yaml"

	content, err := os.ReadFile(kustomizationPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", kustomizationPath, err)
	}

	baseDir := filepath.Dir(kustomizationPath)
	lines := strings.Split(string(content), "\n")
	var missing []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		resource := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
		if resource == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(baseDir, resource)); err != nil {
			missing = append(missing, resource)
		}
	}

	if len(missing) > 0 {
		t.Fatalf("kustomization references missing sample files: %v", missing)
	}
}
