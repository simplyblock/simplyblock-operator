// Validation of the CEL rules compiled into the StorageCluster CRD schema, run
// against a real apiserver. It lives here rather than under internal/webhook
// because there is no webhook involved: the rules are enforced by the apiserver
// itself, and envtest is the only place in the tree that starts one.
//
// The suite installs CRDs and nothing else — no webhook server, no
// ValidatingWebhookConfiguration — so a rejection here can only have come from
// the schema. That is the point of the test: it is what lets the
// enableAtomicity4K-requires-enableChecksumValidation rule live in the CRD
// instead of in a validating webhook of its own.
//
// Every case is written against v1alpha2, which is the storage version. The
// same rules are declared on v1alpha1 and cannot be exercised here, because a
// read at a non-storage version goes through a conversion webhook envtest does
// not run.

package cluster

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// TestStorageClusterCELRejectsAtomic4kWritesWithoutChecksumValidation covers
// every combination of the top-level checksum validation fields.
// enableAtomicity4K is only meaningful as an inline-checksum fallback-path
// escape hatch, so it requires enableChecksumValidation. Every other
// combination is legal.
func TestStorageClusterCELRejectsAtomic4kWritesWithoutChecksumValidation(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name                     string
		enableChecksumValidation *bool
		enableAtomicity4K        *bool
		wantDenied               bool
	}{
		{name: "both unset"},
		{name: "both false", enableChecksumValidation: ptr.To(false), enableAtomicity4K: ptr.To(false)},
		{name: "enableChecksumValidation alone", enableChecksumValidation: ptr.To(true), enableAtomicity4K: ptr.To(false)},
		{name: "both true", enableChecksumValidation: ptr.To(true), enableAtomicity4K: ptr.To(true)},
		{name: "enableAtomicity4K alone", enableChecksumValidation: ptr.To(false), enableAtomicity4K: ptr.To(true), wantDenied: true},
		{name: "enableAtomicity4K with nil enableChecksumValidation", enableAtomicity4K: ptr.To(true), wantDenied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// vcpuCount and maxSubsystemCount are the spec's only required
			// fields, and vcpuCount carries a minimum of its own.
			cluster := &simplyblockv1alpha2.StorageCluster{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-", Namespace: "default"},
				Spec: simplyblockv1alpha2.StorageClusterSpec{
					MaxSubsystemCount:        ptr.To(int32(10)),
					VCPUCount:                ptr.To(int32(6)),
					EnableChecksumValidation: tc.enableChecksumValidation,
					EnableAtomicity4K:        tc.enableAtomicity4K,
				},
			}

			err := apiClient.Create(context.Background(), cluster)
			if tc.wantDenied {
				if err == nil {
					t.Fatal("expected the apiserver to reject the cluster, but it was accepted")
				}
				if !strings.Contains(err.Error(),
					"enableAtomicity4K requires enableChecksumValidation to be true") {
					t.Fatalf("rejected for the wrong reason: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected the apiserver to accept the cluster, got: %v", err)
			}
			if err := apiClient.Delete(context.Background(), cluster); err != nil {
				t.Errorf("cleaning up the cluster: %v", err)
			}
		})
	}
}

// TestStorageClusterChecksumValidationFieldsAreImmutable pins what
// k8s:immutable covers on the top-level checksum fields.
func TestStorageClusterChecksumValidationFieldsAreImmutable(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name                     string
		initialEnableAtomicity4K *bool
		// mutate changes an admitted cluster in the way the test expects the
		// apiserver to refuse.
		mutate  func(*simplyblockv1alpha2.StorageCluster)
		wantErr string
	}{
		{
			name: "changing enableChecksumValidation",
			mutate: func(c *simplyblockv1alpha2.StorageCluster) {
				c.Spec.EnableChecksumValidation = ptr.To(false)
			},
			wantErr: "field is immutable",
		},
		{
			name: "removing enableChecksumValidation",
			mutate: func(c *simplyblockv1alpha2.StorageCluster) {
				c.Spec.EnableChecksumValidation = nil
			},
			wantErr: "field is immutable",
		},
		{
			name: "changing enableAtomicity4K",
			mutate: func(c *simplyblockv1alpha2.StorageCluster) {
				c.Spec.EnableAtomicity4K = ptr.To(true)
			},
			wantErr: "field is immutable",
		},
		{
			name:                     "removing enableAtomicity4K",
			initialEnableAtomicity4K: ptr.To(true),
			mutate: func(c *simplyblockv1alpha2.StorageCluster) {
				c.Spec.EnableAtomicity4K = nil
			},
			wantErr: "field is immutable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enableAtomicity4K := ptr.To(false)
			if tc.initialEnableAtomicity4K != nil {
				enableAtomicity4K = tc.initialEnableAtomicity4K
			}
			cluster := &simplyblockv1alpha2.StorageCluster{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "immutable-", Namespace: "default"},
				Spec: simplyblockv1alpha2.StorageClusterSpec{
					MaxSubsystemCount:        ptr.To(int32(10)),
					VCPUCount:                ptr.To(int32(6)),
					EnableChecksumValidation: ptr.To(true),
					EnableAtomicity4K:        enableAtomicity4K,
				},
			}
			if err := apiClient.Create(context.Background(), cluster); err != nil {
				t.Fatalf("creating the cluster: %v", err)
			}
			t.Cleanup(func() {
				_ = apiClient.Delete(context.Background(), cluster)
			})

			tc.mutate(cluster)
			err := apiClient.Update(context.Background(), cluster)
			if err == nil {
				t.Fatal("expected the apiserver to reject the update, but it was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("rejected for the wrong reason: %v", err)
			}
		})
	}
}

// TestStorageClusterKMSIsImmutableOnceSet pins the rule the CRD redesign moved
// from spec.hashicorpVaultSettings up to the block that now holds every
// provider (design-storagecluster.md §3.1).
//
// The block rather than its members is what carries the rule, because
// switching providers on a live cluster is at least as unsupportable as
// changing one provider's endpoint. A first assignment is allowed, which is the
// once-set semantics §3.2 describes.
func TestStorageClusterKMSIsImmutableOnceSet(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "kms-", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(10)),
			VCPUCount:         ptr.To(int32(6)),
		},
	}
	if err := apiClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating the cluster: %v", err)
	}
	t.Cleanup(func() { _ = apiClient.Delete(ctx, cluster) })

	// Filling it in later is the case the rule allows.
	cluster.Spec.KMS = &simplyblockv1alpha2.KMSSpec{
		Vault: &simplyblockv1alpha2.VaultKMS{BaseURL: "https://vault.example.com:8200"},
	}
	if err := apiClient.Update(ctx, cluster); err != nil {
		t.Fatalf("a first assignment of spec.kms should be accepted, got: %v", err)
	}

	for name, mutate := range map[string]func(*simplyblockv1alpha2.StorageCluster){
		"changing the endpoint": func(c *simplyblockv1alpha2.StorageCluster) {
			c.Spec.KMS.Vault.BaseURL = "https://vault.elsewhere.com:8200"
		},
		"clearing the block": func(c *simplyblockv1alpha2.StorageCluster) {
			c.Spec.KMS = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stored simplyblockv1alpha2.StorageCluster
			if err := apiClient.Get(ctx, client.ObjectKeyFromObject(cluster), &stored); err != nil {
				t.Fatalf("reading the cluster back: %v", err)
			}
			mutate(&stored)
			err := apiClient.Update(ctx, &stored)
			if err == nil {
				t.Fatal("expected the apiserver to reject the update, but it was accepted")
			}
			if !strings.Contains(err.Error(), "kms is immutable once set") {
				t.Fatalf("rejected for the wrong reason: %v", err)
			}
		})
	}
}

// TestStorageClusterDeviceClassIsImmutableFromCreation pins the ninth
// immutable field (§3.2). It is defaulted rather than optional, so it is never
// absent, and the field rule applies from creation with no first assignment to
// allow.
func TestStorageClusterDeviceClassIsImmutableFromCreation(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "class-", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(10)),
			VCPUCount:         ptr.To(int32(6)),
		},
	}
	if err := apiClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating the cluster: %v", err)
	}
	t.Cleanup(func() { _ = apiClient.Delete(ctx, cluster) })

	// The default describes the fleet that exists: NVMe is the only class the
	// backend accepted before 26.4.
	if got := cluster.Spec.DeviceClass; got != simplyblockv1alpha2.StorageClusterDeviceClassNVMe {
		t.Fatalf("spec.deviceClass defaulted to %q, want NVMe", got)
	}

	cluster.Spec.DeviceClass = simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock
	if err := apiClient.Update(ctx, cluster); err == nil {
		t.Fatal("expected the apiserver to refuse a change of device class under a live cluster")
	}
}

// TestStorageClusterVCPUCountMinimum pins the schema floor on spec.vcpuCount
// (test plan I-09).
//
// The floor is a generated value: it lives as a kubebuilder marker on the Go
// field and reaches the apiserver only through config/crd/bases, which the
// four committed copies of the CRD are in turn derived from. Nothing else in
// the tree fails when the marker and the generated schema disagree, so this
// asserts against the CRD an apiserver actually loads rather than against the
// constant.
func TestStorageClusterVCPUCountMinimum(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name       string
		vcpuCount  int32
		wantDenied bool
	}{
		{name: "one below the floor", vcpuCount: 3, wantDenied: true},
		{name: "at the floor", vcpuCount: 4},
		{name: "above the floor", vcpuCount: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &simplyblockv1alpha2.StorageCluster{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "vcpumin-", Namespace: "default"},
				Spec: simplyblockv1alpha2.StorageClusterSpec{
					MaxSubsystemCount: ptr.To(int32(10)),
					VCPUCount:         ptr.To(tc.vcpuCount),
				},
			}

			err := apiClient.Create(context.Background(), cluster)
			if tc.wantDenied {
				if err == nil {
					t.Fatalf("expected the apiserver to reject vcpuCount %d, but it was accepted",
						tc.vcpuCount)
				}
				if !strings.Contains(err.Error(), "should be greater than or equal to 4") {
					t.Fatalf("rejected for the wrong reason: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected the apiserver to accept vcpuCount %d, got: %v", tc.vcpuCount, err)
			}
			if err := apiClient.Delete(context.Background(), cluster); err != nil {
				t.Errorf("cleaning up the cluster: %v", err)
			}
		})
	}
}

// TestStorageClusterStepRejectsAnUnknownValue pins the CEL rule that stands in
// for an Enum marker on status.step, which is a field of a type another module
// declares (design-crd-model.md §3.1).
func TestStorageClusterStepRejectsAnUnknownValue(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "step-", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(10)),
			VCPUCount:         ptr.To(int32(6)),
		},
	}
	if err := apiClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating the cluster: %v", err)
	}
	t.Cleanup(func() { _ = apiClient.Delete(ctx, cluster) })

	cluster.Status.Step.State = "Persisting"
	if err := apiClient.Status().Update(ctx, cluster); err != nil {
		t.Fatalf("a declared step should be accepted, got: %v", err)
	}

	cluster.Status.Step.State = "Teleporting"
	err := apiClient.Status().Update(ctx, cluster)
	if err == nil {
		t.Fatal("expected the apiserver to reject a step no graph declares")
	}
	if !strings.Contains(err.Error(), "unknown step") {
		t.Fatalf("rejected for the wrong reason: %v", err)
	}
}

// TestStorageClusterNameIsBoundedAtALabelsLimit proves that the rule of §19.4
// is enforced by the apiserver and not merely present in the schema.
//
// The name and the reference are the same rule seen from two sides, so both are
// exercised here: a cluster that could not be called this, and a pool naming a
// cluster that could not exist. The bound is a label's 63 bytes rather than the
// 253 the API server allows an object name, because the cluster's name is
// written into storage.simplyblock.io/cluster on every StorageClass the
// operator generates and into io.simplyblock.storagenodeset on every worker it
// claims — and where a name travels into a label, the label's limit binds.
//
// Coverage of the other kinds carrying the rule is the schema enumeration in
// api/v1alpha2/names_test.go, which is what keeps a kind added later from
// escaping it. What this test adds is that the marker bites.
func TestStorageClusterNameIsBoundedAtALabelsLimit(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	legal := strings.Repeat("a", 63)
	overlong := strings.Repeat("a", 64)

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: legal, Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(10)),
			VCPUCount:         ptr.To(int32(6)),
		},
	}
	if err := apiClient.Create(ctx, cluster); err != nil {
		t.Fatalf("a 63-character name is the longest a label carries, so it must be "+
			"accepted, got: %v", err)
	}
	t.Cleanup(func() { _ = apiClient.Delete(ctx, cluster) })

	refused := cluster.DeepCopy()
	refused.Name = overlong
	refused.ResourceVersion = ""
	if err := apiClient.Create(ctx, refused); err == nil {
		t.Error("the apiserver accepted a 64-character cluster name, which every " +
			"StorageClass and worker label derived from it would then be refused for")
		_ = apiClient.Delete(ctx, refused)
	}

	pool := &simplyblockv1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "bound-", Namespace: "default"},
		Spec:       simplyblockv1alpha2.StoragePoolSpec{ClusterRef: overlong},
	}
	if err := apiClient.Create(ctx, pool); err == nil {
		t.Error("the apiserver accepted a clusterRef longer than a StorageCluster name " +
			"may be, which is an immutable reference to an object that cannot exist")
		_ = apiClient.Delete(ctx, pool)
	}
}

// The erasure-coding scheme is one of the seven the control plane accepts, and
// the schema is what says so.
//
// It is CEL rather than a webhook because it is a statement about the document's
// structure: the pair is wrong or right on its own, without reference to any
// cluster, node, or fleet. What it buys is that the refusal arrives at the apply
// rather than from the control plane's cluster create, which is several steps
// and one approval later and leaves a StorageCluster nothing can use behind.
func TestStorageClusterCELAcceptsOnlyTheSupportedErasureCodingSchemes(t *testing.T) {
	apiClient := apiServer(t)

	for _, testCase := range []struct {
		data, parity int32
		denied       bool
	}{
		{data: 1, parity: 0},
		{data: 1, parity: 1},
		{data: 2, parity: 1},
		{data: 4, parity: 1},
		{data: 1, parity: 2},
		{data: 2, parity: 2},
		{data: 4, parity: 2},
		{data: 3, parity: 1, denied: true},
		{data: 8, parity: 2, denied: true},
		{data: 2, parity: 0, denied: true},
		{data: 4, parity: 0, denied: true},
		{data: 1, parity: 3, denied: true},
		{data: 16, parity: 4, denied: true},
	} {
		cluster := &simplyblockv1alpha2.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "stripe-", Namespace: "default"},
			Spec: simplyblockv1alpha2.StorageClusterSpec{
				MaxSubsystemCount: ptr.To(int32(10)),
				VCPUCount:         ptr.To(int32(6)),
				Stripe: &simplyblockv1alpha2.StripeSpec{
					DataChunks: ptr.To(testCase.data), ParityChunks: ptr.To(testCase.parity),
				},
			},
		}

		err := apiClient.Create(context.Background(), cluster)
		switch {
		case testCase.denied && err == nil:
			t.Errorf("%d+%d was accepted, and the control plane refuses it",
				testCase.data, testCase.parity)
		case testCase.denied && !strings.Contains(err.Error(), "erasure-coding scheme"):
			t.Errorf("%d+%d was refused for the wrong reason: %v",
				testCase.data, testCase.parity, err)
		case !testCase.denied && err != nil:
			t.Errorf("%d+%d was refused: %v", testCase.data, testCase.parity, err)
		}
		if err == nil {
			if err := apiClient.Delete(context.Background(), cluster); err != nil {
				t.Fatalf("clean up: %v", err)
			}
		}
	}
}

// A cluster stating half a stripe is stating the control plane's default for the
// other half, so the pair the rule sees is the pair the cluster is created with.
func TestStorageClusterCELReadsAnUnstatedHalfAsTheDefault(t *testing.T) {
	apiClient := apiServer(t)

	for _, stripe := range []*simplyblockv1alpha2.StripeSpec{
		{},
		{DataChunks: ptr.To(int32(2))},
		{ParityChunks: ptr.To(int32(0))},
	} {
		cluster := &simplyblockv1alpha2.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "stripe-half-", Namespace: "default"},
			Spec: simplyblockv1alpha2.StorageClusterSpec{
				MaxSubsystemCount: ptr.To(int32(10)),
				VCPUCount:         ptr.To(int32(6)),
				Stripe:            stripe,
			},
		}

		// {} is 1+1, {dataChunks: 2} is 2+1, and {parityChunks: 0} is 1+0: all
		// three are supported, and a rule reading an absent field as absent
		// rather than as its default would refuse or admit the wrong ones.
		if err := apiClient.Create(context.Background(), cluster); err != nil {
			t.Errorf("a half-stated stripe %+v was refused: %v", stripe, err)
			continue
		}
		if err := apiClient.Delete(context.Background(), cluster); err != nil {
			t.Fatalf("clean up: %v", err)
		}
	}
}
