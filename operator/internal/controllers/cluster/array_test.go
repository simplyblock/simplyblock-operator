// Tests for a multi-attach cluster's storage array: the create parameters,
// registering the array with the control plane, re-registering after a spec
// edit or a rotated login, and what a refusal leaves in status.

package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const arraySecretName = "pure01-login"

func withArray(source simplyblockv1alpha2.StorageArrayCredentialsSource) func(*simplyblockv1alpha2.StorageCluster) {
	return func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.DeviceClass = simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock
		c.Spec.RaidDevices = ptr.To(true)
		creds := simplyblockv1alpha2.StorageArrayCredentials{Source: source}
		switch source {
		case simplyblockv1alpha2.StorageArrayCredentialsSourceSecret:
			creds.SecretRef = &corev1.LocalObjectReference{Name: arraySecretName}
		case simplyblockv1alpha2.StorageArrayCredentialsSourceVaultKV:
			creds.VaultKV = &simplyblockv1alpha2.StorageArrayVaultKVSecret{
				Mount: "secret", Path: "arrays/pure01", Version: ptr.To(int32(3))}
		case simplyblockv1alpha2.StorageArrayCredentialsSourceKMIPSecret:
			creds.KMIPSecret = &simplyblockv1alpha2.StorageArrayKMIPSecret{UID: "42", Field: "api_token"}
		}
		c.Spec.MultiAttach = &simplyblockv1alpha2.MultiAttachSpec{
			FaultTolerance: 2,
			Array: &simplyblockv1alpha2.StorageArraySpec{
				Plugin:      "pure-flasharray",
				Endpoint:    "https://pure01.example",
				Protocol:    simplyblockv1alpha2.StorageArrayProtocolISCSI,
				LunSize:     resource.MustParse("1Ti"),
				CABundle:    "-----BEGIN CERTIFICATE-----x",
				Options:     map[string]string{"eradicate": "true"},
				Credentials: creds,
			},
		}
	}
}

func arraySecret(token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: objectMeta(arraySecretName),
		Data:       map[string][]byte{"api_token": []byte(token)},
	}
}

func registeredArray(reg atlascp.ArrayRegistration) (atlascp.Array, error) {
	return atlascp.Array{ID: "arr-1", Plugin: reg.Plugin, Endpoint: reg.Endpoint,
		HasCredentials: true, CredentialKind: "managed", Protocols: []string{"fc", "iscsi"}}, nil
}

func TestAMultiAttachClusterIsCreatedWithItsFaultTolerance(t *testing.T) {
	r := newClusterReconciler(t, &fakeControlPlane{}, &recorder{})
	params, err := r.creationParams(context.Background(),
		newTestCluster(withArray(simplyblockv1alpha2.StorageArrayCredentialsSourceNone)))
	if err != nil {
		t.Fatal(err)
	}
	if !params.MultiAttach || params.FaultTolerance != 2 || !params.RaidDevices {
		t.Errorf("params = multiAttach %v, ftt %d, raid %v; want true, 2, true",
			params.MultiAttach, params.FaultTolerance, params.RaidDevices)
	}

	params, err = r.creationParams(context.Background(), newTestCluster())
	if err != nil {
		t.Fatal(err)
	}
	if params.MultiAttach || params.FaultTolerance != 0 {
		t.Errorf("a regular cluster was created multi-attach: %+v", params)
	}
}

func TestTheArrayIsRegisteredWithTheSecretsLogin(t *testing.T) {
	var sent []atlascp.ArrayRegistration
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		registerArray: func(id string, reg atlascp.ArrayRegistration) (atlascp.Array, error) {
			if id != testClusterUUID {
				t.Errorf("registered for cluster %s", id)
			}
			sent = append(sent, reg)
			return registeredArray(reg)
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newTestCluster(withArray(simplyblockv1alpha2.StorageArrayCredentialsSourceSecret)), arraySecret("T0KEN"))

	cluster := reconcileCluster(t, r, 1)

	if len(sent) != 1 {
		t.Fatalf("registered %d times, want 1", len(sent))
	}
	reg := sent[0]
	if reg.Plugin != "pure-flasharray" || reg.Endpoint != "https://pure01.example" ||
		reg.LunSizeBytes != 1<<40 || reg.Credentials["api_token"] != "T0KEN" || reg.CredentialRef != nil {
		t.Errorf("registration = %+v", reg)
	}
	if reg.AllowLocalKeyStore != nil {
		t.Errorf("allowLocalKeyStore sent though the spec leaves it out: %v", *reg.AllowLocalKeyStore)
	}
	if reg.Options["protocol"] != "iscsi" || reg.Options["ca_bundle"] != "-----BEGIN CERTIFICATE-----x" ||
		reg.Options["eradicate"] != "true" {
		t.Errorf("options = %v", reg.Options)
	}
	st := cluster.Status.Array
	if st == nil || !st.Registered || st.ID != "arr-1" || st.Message != "" || st.CredentialsVersion == "" ||
		strings.Join(st.Protocols, ",") != "fc,iscsi" || st.ObservedGeneration != cluster.Generation {
		t.Fatalf("status.array = %+v", st)
	}
	if !rec.has(ArrayRegistered) {
		t.Error("no ArrayRegistered event")
	}

	// Nothing changed: not registered again.
	reconcileCluster(t, r, 2)
	if len(sent) != 1 {
		t.Errorf("an unchanged array was registered %d times", len(sent))
	}

	// A rotated login is registered again.
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: testNamespace, Name: arraySecretName}
	if err := r.Get(context.Background(), key, &secret); err != nil {
		t.Fatal(err)
	}
	secret.Data["api_token"] = []byte("R0TATED")
	if err := r.Update(context.Background(), &secret); err != nil {
		t.Fatal(err)
	}
	reconcileCluster(t, r, 1)
	if len(sent) != 2 || sent[1].Credentials["api_token"] != "R0TATED" {
		t.Errorf("the rotated login was not registered: %d registrations", len(sent))
	}
}

// The journal placement reaches the registration: omitted, every node's
// journal is a LUN of its own; Local keeps it on the node's own device; a
// journal LUN may carry its size and the pool it is created in.
func TestTheJournalPlacementIsRegistered(t *testing.T) {
	size := resource.MustParse("64Gi")
	for _, tc := range []struct {
		name    string
		journal *simplyblockv1alpha2.StorageArrayJournal
		onLUN   bool
		size    int64
		pool    string
	}{
		{name: "omitted", onLUN: true},
		{name: "LUN in a fast pool", onLUN: true, size: 64 << 30, pool: "fast",
			journal: &simplyblockv1alpha2.StorageArrayJournal{
				Placement: simplyblockv1alpha2.StorageArrayJournalLUN, Size: &size,
				Options: map[string]string{"pool": "fast"}}},
		{name: "local", journal: &simplyblockv1alpha2.StorageArrayJournal{
			Placement: simplyblockv1alpha2.StorageArrayJournalLocal}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []atlascp.ArrayRegistration
			api := &fakeControlPlane{
				cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
				registerArray: func(_ string, reg atlascp.ArrayRegistration) (atlascp.Array, error) {
					sent = append(sent, reg)
					return registeredArray(reg)
				},
			}
			cluster := newTestCluster(withArray(simplyblockv1alpha2.StorageArrayCredentialsSourceSecret),
				func(c *simplyblockv1alpha2.StorageCluster) { c.Spec.MultiAttach.Array.Journal = tc.journal })
			r := newClusterReconciler(t, api, &recorder{}, cluster, arraySecret("T0KEN"))
			reconcileCluster(t, r, 1)
			if len(sent) != 1 {
				t.Fatalf("registered %d times, want 1", len(sent))
			}
			reg := sent[0]
			if reg.JournalOnLUN != tc.onLUN || reg.JournalSizeBytes != tc.size || reg.JournalOptions["pool"] != tc.pool {
				t.Errorf("journal = %v %d %v, want %v %d pool %q",
					reg.JournalOnLUN, reg.JournalSizeBytes, reg.JournalOptions, tc.onLUN, tc.size, tc.pool)
			}
			if reg.Options["pool"] != "" {
				t.Errorf("the journal pool leaked into the array's options: %v", reg.Options)
			}
		})
	}
}

func TestAnExternalLoginIsNamedNotSent(t *testing.T) {
	for _, tc := range []struct {
		source simplyblockv1alpha2.StorageArrayCredentialsSource
		want   map[string]string
	}{
		{simplyblockv1alpha2.StorageArrayCredentialsSourceVaultKV,
			map[string]string{"kind": "vault-kv", "mount": "secret", "path": "arrays/pure01", "version": "3"}},
		{simplyblockv1alpha2.StorageArrayCredentialsSourceKMIPSecret,
			map[string]string{"kind": "kmip-secret", "uid": "42", "field": "api_token"}},
		{simplyblockv1alpha2.StorageArrayCredentialsSourceNone, nil},
	} {
		t.Run(string(tc.source), func(t *testing.T) {
			var sent atlascp.ArrayRegistration
			api := &fakeControlPlane{
				cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
				registerArray: func(_ string, reg atlascp.ArrayRegistration) (atlascp.Array, error) {
					sent = reg
					return registeredArray(reg)
				},
			}
			r := newClusterReconciler(t, api, &recorder{}, newTestCluster(withArray(tc.source)))
			cluster := reconcileCluster(t, r, 1)
			if len(sent.Credentials) != 0 {
				t.Errorf("credentials sent: %v", sent.Credentials)
			}
			if len(sent.CredentialRef) != len(tc.want) {
				t.Fatalf("credential ref = %v, want %v", sent.CredentialRef, tc.want)
			}
			for k, v := range tc.want {
				if sent.CredentialRef[k] != v {
					t.Errorf("credential ref %s = %q, want %q", k, sent.CredentialRef[k], v)
				}
			}
			if cluster.Status.Array == nil || !cluster.Status.Array.Registered {
				t.Errorf("status.array = %+v", cluster.Status.Array)
			}
		})
	}
}

func TestARefusedRegistrationIsReportedAndRetried(t *testing.T) {
	refuse := true
	calls := 0
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		registerArray: func(_ string, reg atlascp.ArrayRegistration) (atlascp.Array, error) {
			calls++
			if refuse {
				return atlascp.Array{}, errors.New("array plugin 'pure-flasharray' could not connect: 401")
			}
			return registeredArray(reg)
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newTestCluster(withArray(simplyblockv1alpha2.StorageArrayCredentialsSourceSecret)), arraySecret("WR0NG"))

	cluster := reconcileCluster(t, r, 1)
	st := cluster.Status.Array
	if st == nil || st.Registered || !strings.Contains(st.Message, "could not connect") {
		t.Fatalf("status.array = %+v", st)
	}
	if !rec.has(ArrayRegistrationRefused) {
		t.Error("no ArrayRegistrationRefused event")
	}
	if strings.Contains(st.Message, "WR0NG") {
		t.Error("the message carries the login")
	}

	refuse = false
	cluster = reconcileCluster(t, r, 1)
	if calls != 2 || !cluster.Status.Array.Registered || cluster.Status.Array.Message != "" {
		t.Errorf("not retried: %d calls, status %+v", calls, cluster.Status.Array)
	}
}

func TestAMissingLoginSecretIsReported(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{},
		newTestCluster(withArray(simplyblockv1alpha2.StorageArrayCredentialsSourceSecret)))
	cluster := reconcileCluster(t, r, 1)
	st := cluster.Status.Array
	if st == nil || st.Registered || !strings.Contains(st.Message, arraySecretName) {
		t.Fatalf("status.array = %+v", st)
	}
}

func TestAClusterWithoutAnArrayRegistersNone(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	cluster := reconcileCluster(t, newClusterReconciler(t, api, &recorder{}, newTestCluster()), 1)
	if cluster.Status.Array != nil {
		t.Errorf("status.array = %+v", cluster.Status.Array)
	}
	noArray := newTestCluster(withArray(simplyblockv1alpha2.StorageArrayCredentialsSourceNone),
		func(c *simplyblockv1alpha2.StorageCluster) { c.Spec.MultiAttach.Array = nil })
	cluster = reconcileCluster(t, newClusterReconciler(t, api, &recorder{}, noArray), 1)
	if cluster.Status.Array != nil {
		t.Errorf("status.array without an array = %+v", cluster.Status.Array)
	}
}

// Raising lunSize grows every LUN to it, once; a failed growth is reported
// and repeated; lowering it is refused without a call.
func TestRaisingTheLunSizeGrowsEveryLUN(t *testing.T) {
	var grown []int64
	fail := true
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		registerArray: func(_ string, reg atlascp.ArrayRegistration) (atlascp.Array, error) {
			return registeredArray(reg)
		},
		growLUNs: func(id string, size int64) (atlascp.LUNGrowth, error) {
			if id != testClusterUUID {
				t.Errorf("grew the LUNs of cluster %s", id)
			}
			grown = append(grown, size)
			if fail {
				fail = false
				return atlascp.LUNGrowth{Errors: map[string]string{"dev-1": "array resize failed"}}, nil
			}
			return atlascp.LUNGrowth{Grown: true}, nil
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newTestCluster(withArray(simplyblockv1alpha2.StorageArrayCredentialsSourceSecret)), arraySecret("T0KEN"))

	// The first registration records the size the LUNs are created at.
	cluster := reconcileCluster(t, r, 1)
	if st := cluster.Status.Array; st == nil || st.LunSize == nil || st.LunSize.Value() != 1<<40 {
		t.Fatalf("status.array.lunSize = %+v, want 1Ti", cluster.Status.Array)
	}
	if len(grown) != 0 {
		t.Fatalf("grown %v at the first registration", grown)
	}

	setLunSize(t, r, "1536Gi")
	cluster = reconcileCluster(t, r, 1)
	if len(grown) != 1 || grown[0] != 1536<<30 {
		t.Fatalf("grown = %v, want one growth to 1536Gi", grown)
	}
	if st := cluster.Status.Array; st.LunSize.Value() != 1<<40 || !strings.Contains(st.Message, "array resize failed") {
		t.Errorf("after a failed growth: lunSize %s, message %q", st.LunSize, st.Message)
	}
	if !rec.has(ArrayLUNGrowthFailed) {
		t.Error("no ArrayLUNGrowthFailed event")
	}

	// Repeated, and it succeeds.
	cluster = reconcileCluster(t, r, 1)
	if len(grown) != 2 || cluster.Status.Array.LunSize.Value() != 1536<<30 || cluster.Status.Array.Message != "" {
		t.Fatalf("grown = %v, status.array = %+v", grown, cluster.Status.Array)
	}
	if !rec.has(ArrayLUNsGrown) {
		t.Error("no ArrayLUNsGrown event")
	}

	// Nothing more to grow.
	reconcileCluster(t, r, 1)
	if len(grown) != 2 {
		t.Errorf("grown again at an unchanged size: %v", grown)
	}

	// Lowered: refused without a call.
	setLunSize(t, r, "1Ti")
	cluster = reconcileCluster(t, r, 1)
	if len(grown) != 2 || !strings.Contains(cluster.Status.Array.Message, "never shrunk") {
		t.Errorf("a lowered size: grown %v, message %q", grown, cluster.Status.Array.Message)
	}
}

func setLunSize(t *testing.T, r *StorageClusterReconciler, size string) {
	t.Helper()
	var cluster simplyblockv1alpha2.StorageCluster
	key := types.NamespacedName{Namespace: testNamespace, Name: testClusterName}
	if err := r.Get(context.Background(), key, &cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.MultiAttach.Array.LunSize = resource.MustParse(size)
	if err := r.Update(context.Background(), &cluster); err != nil {
		t.Fatal(err)
	}
}

// The opt-in to the local key store travels with the registration.
func TestTheLocalKeyStoreOptInIsRegistered(t *testing.T) {
	var sent []atlascp.ArrayRegistration
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		registerArray: func(_ string, reg atlascp.ArrayRegistration) (atlascp.Array, error) {
			sent = append(sent, reg)
			return registeredArray(reg)
		},
	}
	cluster := newTestCluster(withArray(simplyblockv1alpha2.StorageArrayCredentialsSourceSecret),
		func(c *simplyblockv1alpha2.StorageCluster) {
			c.Spec.MultiAttach.Array.Credentials.AllowLocalKeyStore = ptr.To(true)
		})
	r := newClusterReconciler(t, api, &recorder{}, cluster, arraySecret("T0KEN"))
	reconcileCluster(t, r, 1)
	if len(sent) != 1 || sent[0].AllowLocalKeyStore == nil || !*sent[0].AllowLocalKeyStore {
		t.Fatalf("registrations = %+v, want the opt-in", sent)
	}
}
