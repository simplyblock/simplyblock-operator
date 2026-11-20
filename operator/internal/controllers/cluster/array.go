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
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// reconcileArray registers the cluster's array when the registration in
// status does not match the spec generation and login it was made from, then
// grows its LUNs when spec.multiAttach.array.lunSize was raised. It returns
// the status to record, nil for a cluster without an array.
func (r *StorageClusterReconciler) reconcileArray(
	ctx, readCtx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) *simplyblockv1alpha2.StorageArrayStatus {
	status := r.registerArray(ctx, readCtx, cluster)
	if status == nil {
		return nil
	}
	status = status.DeepCopy()
	if previous := cluster.Status.Array; previous != nil && status.LunSize == nil {
		status.LunSize = previous.LunSize
	}
	r.growLUNs(readCtx, cluster, status)
	return status
}

// growLUNs grows every LUN to spec.multiAttach.array.lunSize once the array
// is registered and the size was raised above status.lunSize. The first
// registration records the size the LUNs are created at. A failed growth is
// repeated on the next pass: LUNs that already have the size are left alone.
func (r *StorageClusterReconciler) growLUNs(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, status *simplyblockv1alpha2.StorageArrayStatus,
) {
	if !status.Registered {
		return
	}
	// A registered array carries no registration message, so what is left
	// is the last growth's, which this pass decides anew.
	status.Message = ""
	want := cluster.Spec.MultiAttach.Array.LunSize
	if status.LunSize == nil {
		status.LunSize = ptr.To(want.DeepCopy())
		return
	}
	switch cmp := want.Cmp(*status.LunSize); {
	case cmp == 0:
		return
	case cmp < 0:
		status.Message = fmt.Sprintf("lunSize %s is below the size the LUNs have, %s; a LUN is never shrunk",
			want.String(), status.LunSize.String())
		return
	}
	growth, err := r.API.GrowLUNs(ctx, cluster.Status.UUID, want.Value())
	if err == nil && len(growth.Errors) > 0 {
		err = fmt.Errorf("%s", joinGrowthErrors(growth.Errors))
	}
	if err != nil {
		status.Message = fmt.Sprintf("growing the LUNs to %s: %v", want.String(), err)
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, ArrayLUNGrowthFailed, ArrayLUNGrowthFailed,
			"The LUNs were not all grown to %s, and the growth is repeated: %v", want.String(), err)
		return
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, ArrayLUNsGrown, ArrayLUNsGrown,
		"Every LUN was grown from %s to %s at the array, and the cluster's devices with them",
		status.LunSize.String(), want.String())
	status.LunSize = ptr.To(want.DeepCopy())
}

// joinGrowthErrors renders the control plane's per-device errors in a stable
// order.
func joinGrowthErrors(errors map[string]string) string {
	keys := make([]string, 0, len(errors))
	for k := range errors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+errors[k])
	}
	return strings.Join(parts, "; ")
}

// registerArray registers the cluster's array when the registration in
// status does not match the spec generation and login it was made from.
func (r *StorageClusterReconciler) registerArray(
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
	if journal := spec.Journal; journal == nil || journal.Placement != simplyblockv1alpha2.StorageArrayJournalLocal {
		reg.JournalOnLUN = true
		if journal != nil {
			if journal.Size != nil {
				reg.JournalSizeBytes = journal.Size.Value()
			}
			if len(journal.Options) > 0 {
				reg.JournalOptions = make(map[string]string, len(journal.Options))
				for k, v := range journal.Options {
					reg.JournalOptions[k] = v
				}
			}
		}
	}

	creds := spec.Credentials
	reg.AllowLocalKeyStore = creds.AllowLocalKeyStore
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
