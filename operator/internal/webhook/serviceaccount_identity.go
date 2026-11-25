// The operator's own identity as the API server sees it: the service-account
// username in the claims of the token mounted into its pod. It lives with the
// admission handlers, because they are what compares a request's user to it.

package webhook

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ServiceAccountTokenPath is where a pod's service-account token is mounted.
const ServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// ServiceAccountUsername returns the username in the subject claim of the
// service-account token at tokenPath. The signature is not checked: the token
// is the pod's own, read from its own mount, and only its claim is wanted.
func ServiceAccountUsername(tokenPath string) (string, error) {
	raw, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", fmt.Errorf("read the service-account token: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(string(raw)), ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("the service-account token at %s is not a JWT", tokenPath)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode the service-account token's claims: %w", err)
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse the service-account token's claims: %w", err)
	}
	if !strings.HasPrefix(claims.Subject, "system:serviceaccount:") {
		return "", fmt.Errorf("the service-account token's subject %q names no service account", claims.Subject)
	}
	return claims.Subject, nil
}
