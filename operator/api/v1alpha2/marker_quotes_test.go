// A guard on the CEL rules written in this package's doc comments: gofmt
// reformats doc comments and turns two single quotes, or two backquotes, into
// one typographic quote, which breaks a rule that wrote an empty string that
// way.
// It lives with the types because it reads their source.

package v1alpha2

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoValidationRuleUsesAPairGofmtRewrites(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "//") || !strings.Contains(trimmed, "+kubebuilder:validation:XValidation") {
				continue
			}
			if strings.Contains(trimmed, "''") || strings.Contains(trimmed, "``") {
				t.Errorf("%s:%d: a validation rule contains '' or ``, which gofmt rewrites into a typographic quote; "+
					"compare with has() instead of an empty string", file, i+1)
			}
		}
	}
}
