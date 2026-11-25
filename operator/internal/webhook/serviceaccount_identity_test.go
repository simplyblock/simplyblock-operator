// Tests of reading the operator's service-account username from its token.

package webhook

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func writeToken(t *testing.T, payload string) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	path := filepath.Join(t.TempDir(), "token")
	token := enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(payload)) + "." + enc([]byte("signature"))
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheUsernameIsTheTokenSubject(t *testing.T) {
	path := writeToken(t, `{"sub":"system:serviceaccount:simplyblock:simplyblock-operator","aud":["x"]}`)
	got, err := ServiceAccountUsername(path)
	if err != nil {
		t.Fatalf("ServiceAccountUsername: %v", err)
	}
	if got != "system:serviceaccount:simplyblock:simplyblock-operator" {
		t.Fatalf("username = %q, want the token's subject", got)
	}
}

// A token that does not name a service account must stop the operator rather
// than leave the guard comparing against an empty username.
func TestATokenWithoutAServiceAccountSubjectIsRefused(t *testing.T) {
	for name, payload := range map[string]string{
		"no subject":   `{"aud":["x"]}`,
		"user subject": `{"sub":"alice"}`,
	} {
		if _, err := ServiceAccountUsername(writeToken(t, payload)); err == nil {
			t.Errorf("%s: no error, want one", name)
		}
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := ServiceAccountUsername(missing); err == nil {
		t.Error("a missing token: no error, want one")
	}
}
