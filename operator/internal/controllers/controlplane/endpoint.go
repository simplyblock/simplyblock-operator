// Where the control plane is: derived for one this cluster hosts, echoed and
// validated for one it is managed by.
//
// This is the field that makes the object useful to anything but a human
// (design-controlplane.md §3.3). Every controller in the operator reaches the
// control plane, and each resolves this endpoint per call through
// [NewEndpointResolver], falling back to the environment only where the object
// has published nothing. One object answers where the control plane is, and a
// change to it reaches every reader without a Deployment rollout.
//
// Resolution is deliberately dumb for the managed case: the Service this install
// creates, in the namespace the ControlPlane is in, on the port it publishes.
// The fully qualified name rather than the short one, because a reader in
// another namespace has to be able to use the same string.

package controlplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
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

// The keys a CA bundle Secret may carry. ca.crt is what cert-manager writes and
// what a kubernetes.io/tls Secret carries; tls.crt covers a bundle somebody
// assembled by hand.
var caBundleKeys = []string{"ca.crt", "tls.crt"}

// localEndpoint is where the management API this install created answers.
func localEndpoint(namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", ComponentWebAPI, namespace, webAPIPort)
}

// credentialsError is what a Secret that is missing or unusable produces. It is
// its own type so the reconciler can emit the CredentialsError event for it and
// EndpointUnreachable for everything else, which are different problems with
// different fixes.
type credentialsError struct{ message string }

func (e *credentialsError) Error() string { return e.message }

// managedAccess is everything needed to reach a remote control plane: where
// it is, what to authenticate with, and what to verify its certificate against.
//
// The three travel together because all three come from the same spec block and
// all three are needed by the same call. Returning the transport rather than the
// CA bytes keeps the trust decision in one place instead of at each probe.
type managedAccess struct {
	endpoint string
	token    string

	// client is nil when the system trust store is what verifies the endpoint,
	// which is what an absent caBundleSecretRef means.
	client *http.Client
}

// resolveManaged reads where a remote control plane is, what to
// authenticate with, and what to verify it against.
//
// The endpoint is validated here as well as by the spec's pattern, because the
// pattern admits a loopback address and this does not. An operator pointed at
// 127.0.0.1 would probe itself, which is the request-forgery shape every
// external endpoint in this group is guarded against.
func resolveManaged(
	ctx context.Context, c client.Reader, cp *simplyblockv1alpha2.ControlPlane,
) (managedAccess, error) {
	external := cp.Spec.Source.Managed
	if external == nil {
		return managedAccess{}, fmt.Errorf("spec.source.managed is not set")
	}

	if err := validateEndpoint(external.Endpoint); err != nil {
		return managedAccess{}, err
	}
	access := managedAccess{endpoint: external.Endpoint}

	transport, err := caBundleTransport(ctx, c, cp)
	if err != nil {
		return managedAccess{}, err
	}
	access.client = transport

	// No reference means no token, which is the in-cluster case the chart writes
	// when it installs the control plane itself.
	if external.CredentialsSecretRef == nil || external.CredentialsSecretRef.Name == "" {
		return access, nil
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: cp.Namespace, Name: external.CredentialsSecretRef.Name}
	if err := c.Get(ctx, key, &secret); err != nil {
		if errors.IsNotFound(err) {
			return managedAccess{}, &credentialsError{message: fmt.Sprintf(
				"Secret %s/%s does not exist, and it is what holds the bearer token this "+
					"control plane is reached with", cp.Namespace, external.CredentialsSecretRef.Name)}
		}
		return managedAccess{}, err
	}

	for _, k := range credentialKeys {
		if value := strings.TrimSpace(string(secret.Data[k])); value != "" {
			access.token = value
			return access, nil
		}
	}
	return managedAccess{}, &credentialsError{message: fmt.Sprintf(
		"Secret %s/%s carries no %s key, so there is no token to authenticate with",
		cp.Namespace, external.CredentialsSecretRef.Name, strings.Join(credentialKeys, " or "))}
}

// caBundleTransport builds the HTTP client that verifies the endpoint against
// the CA the spec names, or nil where it names none.
//
// A Secret that is named and unusable is an error rather than a fall back to the
// system trust store. Naming a CA is a statement that the endpoint is signed by
// it, so quietly verifying against something else would turn a misconfiguration
// into a connection to a control plane nobody vouched for.
func caBundleTransport(
	ctx context.Context, c client.Reader, cp *simplyblockv1alpha2.ControlPlane,
) (*http.Client, error) {
	ref := cp.Spec.Source.Managed.CABundleSecretRef
	if ref == nil || ref.Name == "" {
		return nil, nil
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: cp.Namespace, Name: ref.Name}
	if err := c.Get(ctx, key, &secret); err != nil {
		if errors.IsNotFound(err) {
			return nil, &credentialsError{message: fmt.Sprintf(
				"Secret %s/%s does not exist, and it is what the endpoint's certificate is "+
					"verified against", cp.Namespace, ref.Name)}
		}
		return nil, err
	}

	var bundle []byte
	for _, k := range caBundleKeys {
		if value := secret.Data[k]; len(value) > 0 {
			bundle = value
			break
		}
	}
	if len(bundle) == 0 {
		return nil, &credentialsError{message: fmt.Sprintf(
			"Secret %s/%s carries no %s key, so there is no CA to verify the endpoint against",
			cp.Namespace, ref.Name, strings.Join(caBundleKeys, " or "))}
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, &credentialsError{message: fmt.Sprintf(
			"Secret %s/%s holds no PEM certificate this operator could parse",
			cp.Namespace, ref.Name)}
	}

	return &http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}, nil
}

// validateEndpoint refuses the addresses a remote control plane must not be
// at. It is the outbound-URL guard every other outbound endpoint in this group
// carries, applied at the one place the endpoint is read.
func validateEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("spec.source.managed.endpoint is not a URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("spec.source.managed.endpoint has scheme %q, want http or https", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("spec.source.managed.endpoint names no host")
	}
	if isLoopbackOrLinkLocal(host) {
		return fmt.Errorf(
			"spec.source.managed.endpoint names %q, which resolves inside the operator's own "+
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
