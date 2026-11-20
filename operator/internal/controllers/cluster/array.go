// The storage array of a multi-attach cluster: registering it with the control
// plane once the cluster exists, and again whenever its spec or its login
// changes.
//
// It lives beside the steady-state sync rather than in the creation machine
// because the registration is not a one-off: an endpoint moves, a pool is
// renamed, and a login is rotated, and each of those is the same call again.
// The control plane connects to the array before it saves anything, so a
// refused registration leaves the previous one in force.

package cluster

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	atlascp "github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// reconcileArray registers the cluster's array when the registration in
// status does not match the spec generation and login it was made from. It
// returns the status to record, nil for a cluster without an array.
func (r *StorageClusterReconciler) reconcileArray(
	ctx, readCtx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) *simplyblockv1alpha2.StorageArrayStatus {
	ma := cluster.Spec.MultiAttach
	if ma == nil || ma.Array == nil {
		return nil
	}
	spec := ma.Array
	previous := cluster.Status.Array
	status := &simplyblockv1alpha2.StorageArrayStatus{ObservedGeneration: cluster.Generation}
	if previous != nil {
		status.ID = previous.ID
		status.Protocols = previous.Protocols
	}

	reg, version, err := r.arrayRegistration(ctx, cluster, spec)
	if err != nil {
		status.Message = err.Error()
		if previous == nil || previous.Message != status.Message {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
				ArrayRegistrationRefused, ArrayRegistrationRefused,
				"The storage array was not registered: %v", err)
		}
		return status
	}
	status.CredentialsVersion = version

	if previous != nil && previous.Registered && previous.ObservedGeneration == cluster.Generation &&
		previous.CredentialsVersion == version {
		return previous
	}

	array, err := r.API.RegisterArray(readCtx, cluster.Status.UUID, reg)
	if err != nil {
		// The control plane's reason (could not connect, login refused, a
		// capability the cluster needs) is what the administrator acts on. It
		// never carries the login: the request body is not part of the error.
		status.Message = err.Error()
		if previous == nil || previous.Message != status.Message {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
				ArrayRegistrationRefused, ArrayRegistrationRefused,
				"The control plane refused the storage array: %v", err)
		}
		return status
	}
	status.ID = array.ID
	status.Protocols = array.Protocols
	status.Registered = true
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal,
		ArrayRegistered, ArrayRegistered,
		"Registered %s at %s with the control plane (credentials: %s)",
		spec.Plugin, displayEndpoint(spec.Endpoint), credentialKind(array))
	return status
}

// arrayRegistration is the request for the spec, and the resource version of
// the login Secret it read (empty for any other credential source).
func (r *StorageClusterReconciler) arrayRegistration(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
	spec *simplyblockv1alpha2.StorageArraySpec,
) (atlascp.ArrayRegistration, string, error) {
	reg := atlascp.ArrayRegistration{
		Plugin:       spec.Plugin,
		Endpoint:     spec.Endpoint,
		Options:      map[string]string{},
		LunSizeBytes: spec.LunSize.Value(),
	}
	for k, v := range spec.Options {
		reg.Options[k] = v
	}
	if spec.Protocol != "" {
		reg.Options["protocol"] = string(spec.Protocol)
	}
	if spec.CABundle != "" {
		reg.Options["ca_bundle"] = spec.CABundle
	}

	creds := spec.Credentials
	switch creds.Source {
	case simplyblockv1alpha2.StorageArrayCredentialsSourceSecret:
		if creds.SecretRef == nil || creds.SecretRef.Name == "" {
			return reg, "", fmt.Errorf("credentials.secretRef names no Secret")
		}
		var secret corev1.Secret
		key := types.NamespacedName{Namespace: cluster.Namespace, Name: creds.SecretRef.Name}
		if err := r.Get(ctx, key, &secret); err != nil {
			return reg, "", fmt.Errorf("read the login Secret %q: %w", creds.SecretRef.Name, err)
		}
		if len(secret.Data) == 0 {
			return reg, "", fmt.Errorf("the login Secret %q has no keys", creds.SecretRef.Name)
		}
		reg.Credentials = make(map[string]string, len(secret.Data))
		for k, v := range secret.Data {
			reg.Credentials[k] = string(v)
		}
		return reg, secret.ResourceVersion, nil
	case simplyblockv1alpha2.StorageArrayCredentialsSourceVaultKV:
		v := creds.VaultKV
		if v == nil {
			return reg, "", fmt.Errorf("credentials.vaultKV is not set")
		}
		reg.CredentialRef = map[string]string{"kind": "vault-kv", "mount": v.Mount, "path": v.Path}
		if v.Version != nil {
			reg.CredentialRef["version"] = strconv.Itoa(int(*v.Version))
		}
	case simplyblockv1alpha2.StorageArrayCredentialsSourceKMIPSecret:
		k := creds.KMIPSecret
		if k == nil {
			return reg, "", fmt.Errorf("credentials.kmipSecret is not set")
		}
		reg.CredentialRef = map[string]string{"kind": "kmip-secret", "uid": k.UID}
		if k.Field != "" {
			reg.CredentialRef["field"] = k.Field
		}
	}
	return reg, "", nil
}

func displayEndpoint(endpoint string) string {
	if endpoint == "" {
		return "its default endpoint"
	}
	return endpoint
}

func credentialKind(array atlascp.Array) string {
	if !array.HasCredentials {
		return "none"
	}
	return array.CredentialKind
}
