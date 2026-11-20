// The runner's per-boot SSH key for the guest: written where the image's ssh
// wrapper reads it, private to the container, and matching what the guest is
// told to accept.

package runner

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestDebugSSHKeyIsAPrivatePairTheGuestAccepts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ssh")
	authorized, err := PrepareDebugSSH(dir)
	if err != nil {
		t.Fatalf("PrepareDebugSSH: %v", err)
	}
	if authorized != filepath.Join(dir, DebugSSHAuthorizedKeys) {
		t.Errorf("authorized keys at %s", authorized)
	}

	private := filepath.Join(dir, DebugSSHPrivateKey)
	info, err := os.Stat(private)
	if err != nil {
		t.Fatalf("private key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("private key mode %v, want 0600", info.Mode().Perm())
	}
	pem, err := os.ReadFile(private)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		t.Fatalf("the private key is not one ssh reads: %v", err)
	}
	line, err := os.ReadFile(authorized)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil {
		t.Fatalf("authorized_keys does not parse: %v", err)
	}
	if string(pub.Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Error("the guest is told to accept a key the runner does not hold")
	}
}

// A new boot gets a new key: one left from an earlier boot is replaced.
func TestDebugSSHKeyIsNewEveryBoot(t *testing.T) {
	dir := t.TempDir()
	if _, err := PrepareDebugSSH(dir); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, DebugSSHAuthorizedKeys))
	if _, err := PrepareDebugSSH(dir); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, DebugSSHAuthorizedKeys))
	if string(first) == string(second) {
		t.Error("a second boot kept the first boot's key")
	}
}
