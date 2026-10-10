// The per-boot SSH key the runner gives the guest when debug SSH is on. The
// private half stays in the runner container, where the image's ssh wrapper
// uses it, so only someone who can exec into the pod can log in, and no secret
// has to be managed.

package runner

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// File names under the debug SSH directory.
const (
	DebugSSHPrivateKey     = "id_ed25519"
	DebugSSHAuthorizedKeys = "authorized_keys"
)

// PrepareDebugSSH writes a fresh key pair into dir and returns the path of the
// authorized_keys line the guest is to accept.
func PrepareDebugSSH(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "mds-runner")
	if err != nil {
		return "", fmt.Errorf("encoding the debug SSH key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, DebugSSHPrivateKey), pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	authorized := filepath.Join(dir, DebugSSHAuthorizedKeys)
	if err := os.WriteFile(authorized, ssh.MarshalAuthorizedKey(sshPub), 0o600); err != nil {
		return "", err
	}
	return authorized, nil
}
