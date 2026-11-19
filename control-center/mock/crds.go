package main

// crds.go serves the simplyblock CustomResourceDefinitions. The console reads
// a CRD's openAPIV3Schema for its x-kubernetes-validations (which fields are
// immutable), so the mock loads the real, generated CRD YAML from
// operator/config/crd/bases instead of inventing a schema. The directory is
// found relative to the working directory or this source file, or given with
// --crd-dir. A build without it (the container image) serves an empty list.

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// crdDirCandidates are tried in order, relative to the working directory, when
// no --crd-dir is given.
var crdDirCandidates = []string{
	"../../operator/config/crd/bases",
	"operator/config/crd/bases",
	"../operator/config/crd/bases",
	"/crds",
}

// findCRDDir returns the first existing candidate, or the empty string.
func findCRDDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	candidates := append([]string{}, crdDirCandidates...)
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(file), "..", "..", "operator", "config", "crd", "bases"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

// loadCRDs reads every CustomResourceDefinition document in dir's `*.yaml`
// files into the store and returns how many it created.
func loadCRDs(st *Store, dir string) int {
	d, ok := st.Lookup(apiExtGroup, "v1", "customresourcedefinitions")
	if !ok || dir == "" {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("crds: %v", err)
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, doc := range strings.Split(string(data), "\n---") {
			var obj map[string]any
			if err := yaml.Unmarshal([]byte(doc), &obj); err != nil || getStr(obj, "kind") != "CustomResourceDefinition" {
				continue
			}
			obj = jsonShape(obj).(map[string]any)
			if _, err := st.Create(d, "", obj); err == nil {
				n++
			}
		}
	}
	return n
}

// jsonShape converts a YAML-decoded value into the JSON shape the store
// keeps everywhere else: string-keyed maps and float64 numbers.
func jsonShape(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = jsonShape(vv)
		}
		return m
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			if ks, ok := k.(string); ok {
				m[ks] = jsonShape(vv)
			}
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, vv := range t {
			s[i] = jsonShape(vv)
		}
		return s
	case int:
		return float64(t)
	case int64:
		return float64(t)
	default:
		return v
	}
}
