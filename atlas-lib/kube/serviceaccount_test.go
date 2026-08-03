package kube

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServiceAccountToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("  a-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	old := ServiceAccountTokenPath
	ServiceAccountTokenPath = path
	t.Cleanup(func() { ServiceAccountTokenPath = old })

	got, err := ServiceAccountToken()
	if err != nil {
		t.Fatalf("ServiceAccountToken: %v", err)
	}
	if got != "a-token" {
		t.Errorf("token = %q, want %q (trimmed)", got, "a-token")
	}
}

func TestServiceAccountTokenMissing(t *testing.T) {
	old := ServiceAccountTokenPath
	ServiceAccountTokenPath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { ServiceAccountTokenPath = old })

	if _, err := ServiceAccountToken(); err == nil {
		t.Error("expected an error for a missing token file")
	}
}
