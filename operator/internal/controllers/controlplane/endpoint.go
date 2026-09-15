// Where the control plane is: derived for a managed one, echoed and validated
// for an external one.
//
// This is the field that makes the object useful to anything but a human
// (design-controlplane.md §3.3). Every controller in the operator reaches the
// control plane, and today every one of them resolves the endpoint from an
// environment variable. In the target they read the endpoint this object
// publishes, so that one object answers where the control plane is and a change
// to it reaches every reader without a Deployment rollout.
//
// Resolution is deliberately dumb for the managed case: the Service this install
// creates, in the namespace the ControlPlane is in, on the port it publishes.
// The fully qualified name rather than the short one, because a reader in
// another namespace has to be able to use the same string.

package controlplane

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The keys a credentials Secret may carry. Two are accepted because the two
// conventions are both in use in this repository, and refusing one of them would
// be a rename disguised as a validation.
var credentialKeys = []string{"token", "secret"}

// managedEndpoint is where the management API this install created answers.
func managedEndpoint(namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", ComponentWebAPI, namespace, webAPIPort)
}

// credentialsError is what a Secret that is missing or unusable produces. It is
// its own type so the reconciler can emit the CredentialsError event for it and
// EndpointUnreachable for everything else, which are different problems with
// different fixes.
type credentialsError struct{ message string }

func (e *credentialsError) Error() string { return e.message }

// resolveExternal reads the endpoint and the bearer token of an external control
// plane.
//
// The endpoint is validated here as well as by the spec's pattern, because the
// pattern admits a loopback address and this does not. An operator pointed at
// 127.0.0.1 would probe itself, which is the request-forgery shape every
// external endpoint in this group is guarded against.
func resolveExternal(
	ctx context.Context, c client.Reader, cp *simplyblockv1alpha2.ControlPlane,
) (endpoint, token string, err error) {
	external := cp.Spec.Source.External
	if external == nil {
		return "", "", fmt.Errorf("spec.source.external is not set")
	}

	if err := validateEndpoint(external.Endpoint); err != nil {
		return "", "", err
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: cp.Namespace, Name: external.CredentialsSecretRef.Name}
	if err := c.Get(ctx, key, &secret); err != nil {
		if errors.IsNotFound(err) {
			return "", "", &credentialsError{message: fmt.Sprintf(
				"Secret %s/%s does not exist, and it is what holds the bearer token this "+
					"control plane is reached with", cp.Namespace, external.CredentialsSecretRef.Name)}
		}
		return "", "", err
	}

	for _, k := range credentialKeys {
		if value := strings.TrimSpace(string(secret.Data[k])); value != "" {
			return external.Endpoint, value, nil
		}
	}
	return "", "", &credentialsError{message: fmt.Sprintf(
		"Secret %s/%s carries no %s key, so there is no token to authenticate with",
		cp.Namespace, external.CredentialsSecretRef.Name, strings.Join(credentialKeys, " or "))}
}

// validateEndpoint refuses the addresses an external control plane must not be
// at. It is the outbound-URL guard every other external endpoint in this group
// carries, applied at the one place the endpoint is read.
func validateEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("spec.source.external.endpoint is not a URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("spec.source.external.endpoint has scheme %q, want http or https", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("spec.source.external.endpoint names no host")
	}
	if isLoopbackOrLinkLocal(host) {
		return fmt.Errorf(
			"spec.source.external.endpoint names %q, which resolves inside the operator's own "+
				"pod rather than to a control plane", host)
	}
	return nil
}

// isLoopbackOrLinkLocal reports the hosts an external endpoint may not name.
// They are matched by spelling rather than by resolution, because resolving a
// name at admission time answers for the moment of admission and not for the
// life of the object.
func isLoopbackOrLinkLocal(host string) bool {
	lowered := strings.ToLower(host)
	switch {
	case lowered == "localhost", strings.HasSuffix(lowered, ".localhost"):
		return true
	case strings.HasPrefix(lowered, "127."), lowered == "::1", lowered == "[::1]":
		return true
	case strings.HasPrefix(lowered, "169.254."):
		return true
	default:
		return false
	}
}
