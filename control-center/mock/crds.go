package main

// The operator's CRDs, served as apiextensions.k8s.io/v1 objects so the
// console can read field descriptions, enums, and immutability rules from the
// same files the operator installs (operator/config/crd/bases). The mock does
// not validate objects against them; it only serves them.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

const apiextGroup = "apiextensions.k8s.io"

// loadCRDs reads every YAML file in dir into the store as a
// CustomResourceDefinition and reports how many it loaded.
func (s *server) loadCRDs(dir string) (int, error) {
	d, ok := s.store.Lookup(apiextGroup, "v1", "customresourcedefinitions")
	if !ok {
		return 0, fmt.Errorf("customresourcedefinitions are not registered")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return n, err
		}
		var obj map[string]any
		if err := yaml.Unmarshal(raw, &obj); err != nil {
			return n, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if getStr(obj, "kind") != "CustomResourceDefinition" {
			continue
		}
		if _, err := s.store.Create(d, "", obj); err != nil {
			return n, fmt.Errorf("%s: %s", e.Name(), err.Message)
		}
		n++
	}
	return n, nil
}
