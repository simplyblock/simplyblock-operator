// The pure functions that derive what the control plane is asked for from what
// the spec says, and the two reconcile paths that are one branch each.
//
// Each of these is a place where an absent field has to mean something
// definite. A nil stripe block is one parity chunk rather than zero, a nil
// threshold is the control plane's own default rather than an alarm at zero
// percent, and an absent concurrency limit is one worker rather than none.
// Every one of those defaults is invisible in the spec and visible only in what
// the cluster then does.

package cluster

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

func TestEffectiveConcurrentRestarts(t *testing.T) {
	for name, tc := range map[string]struct {
		spec, faultTolerance *int32
		want                 int32
	}{
		"below the fault tolerance: the spec wins": {ptr.To(int32(2)), ptr.To(int32(4)), 2},
		"above the fault tolerance: clamped":       {ptr.To(int32(4)), ptr.To(int32(2)), 2},
		"equal to the fault tolerance: no clamp":   {ptr.To(int32(3)), ptr.To(int32(3)), 3},
		"neither stated: one worker at a time":     {nil, nil, 1},
		"no fault tolerance reported: no clamp":    {ptr.To(int32(5)), nil, 5},
		"a fault tolerance of zero: no clamp":      {ptr.To(int32(5)), ptr.To(int32(0)), 5},
		"a spec of zero is read as unstated":       {ptr.To(int32(0)), ptr.To(int32(4)), 1},
	} {
		t.Run(name, func(t *testing.T) {
			got := effectiveConcurrentRestarts(tc.spec, tc.faultTolerance)
			if got == nil || *got != tc.want {
				t.Errorf("effectiveConcurrentRestarts = %v, want %d", got, tc.want)
			}
		})
	}
}

// A stripe nobody stated is one data chunk and one parity chunk, which is what
// the control plane defaults to. Zero would ask for a layout that cannot hold
// anything.
func TestStripeChunksDefaultToOneRatherThanZero(t *testing.T) {
	if got := stripeDataChunks(nil); got != 1 {
		t.Errorf("stripeDataChunks(nil) = %d, want 1", got)
	}
	if got := StripeParityChunks(nil); got != 1 {
		t.Errorf("StripeParityChunks(nil) = %d, want 1", got)
	}
	stripe := &simplyblockv1alpha2.StripeSpec{
		DataChunks:   ptr.To(int32(4)),
		ParityChunks: ptr.To(int32(2)),
	}
	if got := stripeDataChunks(stripe); got != 4 {
		t.Errorf("stripeDataChunks = %d, want 4", got)
	}
	if got := StripeParityChunks(stripe); got != 2 {
		t.Errorf("StripeParityChunks = %d, want 2", got)
	}
}

// A threshold nobody set is zero on the wire, which the control plane reads as
// a request to use its own default. An alarm level of zero percent is not
// expressible and is not meant to be.
func TestAnUnstatedThresholdIsZeroOnTheWire(t *testing.T) {
	if got := capacityThreshold(nil); got != 0 {
		t.Errorf("capacityThreshold(nil) = %d, want 0", got)
	}
	if got := provisionedCapacityThreshold(nil); got != 0 {
		t.Errorf("provisionedCapacityThreshold(nil) = %d, want 0", got)
	}
	threshold := &simplyblockv1alpha2.CapacityThresholdSpec{
		Capacity:            ptr.To(int64(75)),
		ProvisionedCapacity: ptr.To(int64(150)),
	}
	if got := capacityThreshold(threshold); got != 75 {
		t.Errorf("capacityThreshold = %d, want 75", got)
	}
	if got := provisionedCapacityThreshold(threshold); got != 150 {
		t.Errorf("provisionedCapacityThreshold = %d, want 150", got)
	}
}

// A key store the operator cannot reach is a configuration error the user can
// fix, and it is refused before a cluster is created rather than after.
func TestAVaultURLThatIsNotExternalIsRefused(t *testing.T) {
	_, err := vaultConfig(&simplyblockv1alpha2.KMSSpec{
		Vault: &simplyblockv1alpha2.VaultKMS{Endpoint: "https://127.0.0.1:8200"},
	})
	if err == nil {
		t.Error("a loopback vault endpoint was accepted")
	}

	// An address literal rather than a name, because the check resolves a name
	// and a test must not depend on DNS. 203.0.113.0/24 is TEST-NET-3, which
	// is routable as far as the check is concerned and reaches nothing.
	got, err := vaultConfig(&simplyblockv1alpha2.KMSSpec{
		Vault: &simplyblockv1alpha2.VaultKMS{Endpoint: "https://203.0.113.10:8200"},
	})
	if err != nil {
		t.Fatalf("an external vault endpoint was refused: %v", err)
	}
	if got == nil || got.BaseURL != "https://203.0.113.10:8200" {
		t.Errorf("vaultConfig = %+v, want the endpoint carried through", got)
	}
}

// An absent key store is absent on the wire rather than an empty block, which
// is what stops the control plane being told to store keys nowhere.
func TestAnAbsentKeyStoreSendsNothing(t *testing.T) {
	for name, kms := range map[string]*simplyblockv1alpha2.KMSSpec{
		"no block":       nil,
		"no provider":    {},
		"an empty vault": {Vault: &simplyblockv1alpha2.VaultKMS{}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := vaultConfig(kms)
			if err != nil {
				t.Fatalf("vaultConfig: %v", err)
			}
			if got != nil {
				t.Errorf("vaultConfig = %+v, want nothing sent", got)
			}
		})
	}
}

// The backup store's credentials come out of the Secret it names, and what is
// sent is the location and the keys — never the four fields describing how a
// copy is taken, which the control plane now defaults for itself.
func TestTheBackupStoreResolvesItsCredentials(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: objectMeta("backup-credentials"),
		Data: map[string][]byte{
			"access_key_id":     []byte("the-key"),
			"secret_access_key": []byte("the-secret"),
		},
	}
	cluster := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.Backup = &simplyblockv1alpha2.BackupStoreSpec{
			Endpoint:             "https://203.0.113.20:9000",
			Bucket:               "simplyblock-backups",
			Prefix:               "production/",
			Region:               "eu-central-1",
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "backup-credentials"},
		}
	})
	r := newClusterReconciler(t, &fakeControlPlane{}, &recorder{}, cluster, secret)

	got, err := r.backupConfig(context.Background(), cluster)
	if err != nil {
		t.Fatalf("backupConfig: %v", err)
	}
	if got.AccessKeyID != "the-key" || got.SecretAccessKey != "the-secret" {
		t.Errorf("the credentials were not read from the Secret: %+v", got)
	}
	if got.LocalEndpoint != "https://203.0.113.20:9000" || got.Bucket != "simplyblock-backups" ||
		got.Prefix != "production/" || got.Region != "eu-central-1" {
		t.Errorf("the store's location was not carried: %+v", got)
	}
	if got.SnapshotBackups != nil || got.WithCompression != nil ||
		got.LocalTesting != nil || got.SecondaryTarget != nil {
		t.Errorf("the operator is still sending how a copy is taken: %+v", got)
	}
}

// A Secret missing one of the two keys is as unusable as a missing Secret, and
// is refused rather than sent half-filled.
func TestABackupSecretMissingAKeyIsRefused(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: objectMeta("backup-credentials"),
		Data:       map[string][]byte{"access_key_id": []byte("the-key")},
	}
	cluster := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.Backup = &simplyblockv1alpha2.BackupStoreSpec{
			Endpoint:             "https://203.0.113.20:9000",
			Bucket:               "simplyblock-backups",
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "backup-credentials"},
		}
	})
	r := newClusterReconciler(t, &fakeControlPlane{}, &recorder{}, cluster, secret)

	if _, err := r.backupConfig(context.Background(), cluster); err == nil {
		t.Error("a Secret with no secret_access_key was accepted")
	}
}

// The finalizer goes on before anything else happens, because it is what
// guarantees the backend cluster is deleted when the object is.
func TestTheFinalizerIsAddedOnTheFirstReconcile(t *testing.T) {
	fresh := newUncreatedCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Finalizers = nil
	})
	api := &fakeControlPlane{}
	r := newClusterReconciler(t, api, &recorder{}, fresh)

	cluster := reconcileCluster(t, r, 1)
	if len(cluster.Finalizers) != 1 || cluster.Finalizers[0] != Finalizer {
		t.Errorf("finalizers = %v, want only %q", cluster.Finalizers, Finalizer)
	}
	if api.createCalls != 0 {
		t.Error("the first pass did something other than take the finalizer")
	}
}

// An object that is gone is not an error and not a requeue: there is nothing
// to reconcile toward.
func TestReconcilingAClusterThatIsGoneDoesNothing(t *testing.T) {
	r := newClusterReconciler(t, &fakeControlPlane{}, &recorder{})

	key := types.NamespacedName{Namespace: testNamespace, Name: "no-such-cluster"}
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want no requeue for an object that is gone",
			result.RequeueAfter)
	}
}

// An upgrade Secret with half its contents names no cluster, so the creation
// falls through to making one rather than adopting nothing.
func TestAnIncompleteUpgradeSecretFallsThroughToCreation(t *testing.T) {
	api := &fakeControlPlane{
		create: func(utils.ClusterAddParams) (webapi.ClusterResponse, error) {
			// A creation response carries the secret the control plane just
			// minted for the cluster. Only the list response omits it.
			reading := activeCluster()
			reading.Secret = testClusterSecret
			return reading, nil
		},
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	halfFilled := &corev1.Secret{
		ObjectMeta: objectMeta("simplyblock-" + testClusterName + "-upgrade"),
		Data:       map[string][]byte{"uuid": []byte(testClusterUUID)},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, newUncreatedCluster(), halfFilled)

	cluster := reconcileCluster(t, r, 6)
	if cluster.Status.UUID == "" {
		t.Fatalf("the cluster was never created (step %q, message %q)",
			cluster.Status.Step.State, cluster.Status.Message)
	}
	if api.createCalls != 1 {
		t.Errorf("the control plane was asked to create %d clusters, want 1", api.createCalls)
	}
	if rec.has(ClusterAdopted) {
		t.Error("a half-filled upgrade Secret was treated as an adoption")
	}
}

// The per-cluster Secret is owned by the object that produced it, so deleting
// the cluster takes its credentials with it rather than leaving them behind.
func TestTheClusterSecretIsOwnedByTheCluster(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	cluster := newTestCluster()
	r := newClusterReconciler(t, api, &recorder{}, cluster)

	found := adoption{UUID: testClusterUUID, Secret: testClusterSecret}
	if err := r.writeClusterSecret(context.Background(), cluster, found); err != nil {
		t.Fatalf("writeClusterSecret: %v", err)
	}

	var secret corev1.Secret
	key := types.NamespacedName{
		Namespace: testNamespace,
		Name:      "simplyblock-cluster-" + testClusterName,
	}
	if err := r.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("the per-cluster Secret was not written: %v", err)
	}
	if !metav1.IsControlledBy(&secret, cluster) {
		t.Errorf("ownerReferences = %v, want the cluster to own its Secret",
			secret.OwnerReferences)
	}
	if string(secret.Data["secret"]) != testClusterSecret {
		t.Errorf("the Secret does not carry the cluster's secret")
	}
}
