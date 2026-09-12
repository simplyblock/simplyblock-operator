// Validation of the CEL rules compiled into the StorageCluster CRD schema, run
// against a real apiserver. It lives here rather than under internal/webhook
// because there is no webhook involved: the rules are enforced by the apiserver
// itself, and envtest is the only place in the tree that starts one.
//
// The suite installs CRDs and nothing else -- no webhook server, no
// ValidatingWebhookConfiguration -- so a rejection here can only have come from
// the schema. That is the point of the test: it is what lets the
// enableAtomic4kWrites-requires-enableChecksumValidation rule live in the CRD instead of in a
// validating webhook of its own.
package controller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestStorageClusterCELRejectsAtomic4kWritesWithoutChecksumValidation covers
// every combination of the top-level checksum validation fields.
// enableAtomic4kWrites is only meaningful as an inline-checksum fallback-path
// escape hatch, so it requires enableChecksumValidation. Every other
// combination is legal.
func TestStorageClusterCELRejectsAtomic4kWritesWithoutChecksumValidation(t *testing.T) {
	if err := simplyblockv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("adding the simplyblock scheme: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	apiClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}

	for _, tc := range []struct {
		name                     string
		enableChecksumValidation *bool
		enableAtomic4kWrites     *bool
		wantDenied               bool
	}{
		{name: "both unset"},
		{name: "both false", enableChecksumValidation: ptr.To(false), enableAtomic4kWrites: ptr.To(false)},
		{name: "enableChecksumValidation alone", enableChecksumValidation: ptr.To(true), enableAtomic4kWrites: ptr.To(false)},
		{name: "both true", enableChecksumValidation: ptr.To(true), enableAtomic4kWrites: ptr.To(true)},
		{name: "enableAtomic4kWrites alone", enableChecksumValidation: ptr.To(false), enableAtomic4kWrites: ptr.To(true), wantDenied: true},
		{name: "enableAtomic4kWrites with nil enableChecksumValidation", enableAtomic4kWrites: ptr.To(true), wantDenied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// vcpuCount and maxSubsystemCount are the spec's only required
			// fields, and vcpuCount carries a minimum of its own.
			spec := simplyblockv1alpha1.StorageClusterSpec{
				MaxSubsystemCount:        ptr.To(int32(10)),
				VCPUCount:                ptr.To(int32(6)),
				EnableChecksumValidation: tc.enableChecksumValidation,
				EnableAtomic4kWrites:     tc.enableAtomic4kWrites,
			}
			cluster := &simplyblockv1alpha1.StorageCluster{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-", Namespace: "default"},
				Spec:       spec,
			}

			err := apiClient.Create(context.Background(), cluster)
			if tc.wantDenied {
				if err == nil {
					t.Fatal("expected the apiserver to reject the cluster, but it was accepted")
				}
				if !strings.Contains(err.Error(), "enableAtomic4kWrites requires enableChecksumValidation to be true") {
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

// TestStorageClusterChecksumValidationFieldsAreImmutable pins what k8s:immutable
// covers on the top-level checksum fields.
func TestStorageClusterChecksumValidationFieldsAreImmutable(t *testing.T) {
	if err := simplyblockv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("adding the simplyblock scheme: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	apiClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}

	for _, tc := range []struct {
		name                        string
		initialEnableAtomic4kWrites *bool
		// mutate changes an admitted cluster in the way the test expects the
		// apiserver to refuse.
		mutate  func(*simplyblockv1alpha1.StorageCluster)
		wantErr string
	}{
		{
			name: "changing enableChecksumValidation",
			mutate: func(c *simplyblockv1alpha1.StorageCluster) {
				c.Spec.EnableChecksumValidation = ptr.To(false)
			},
			wantErr: "field is immutable",
		},
		{
			name: "removing enableChecksumValidation",
			mutate: func(c *simplyblockv1alpha1.StorageCluster) {
				c.Spec.EnableChecksumValidation = nil
			},
			wantErr: "field is immutable",
		},
		{
			name: "changing enableAtomic4kWrites",
			mutate: func(c *simplyblockv1alpha1.StorageCluster) {
				c.Spec.EnableAtomic4kWrites = ptr.To(true)
			},
			wantErr: "field is immutable",
		},
		{
			name:                        "removing enableAtomic4kWrites",
			initialEnableAtomic4kWrites: ptr.To(true),
			mutate: func(c *simplyblockv1alpha1.StorageCluster) {
				c.Spec.EnableAtomic4kWrites = nil
			},
			wantErr: "field is immutable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enableAtomic4kWrites := ptr.To(false)
			if tc.initialEnableAtomic4kWrites != nil {
				enableAtomic4kWrites = tc.initialEnableAtomic4kWrites
			}
			cluster := &simplyblockv1alpha1.StorageCluster{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "immutable-", Namespace: "default"},
				Spec: simplyblockv1alpha1.StorageClusterSpec{
					MaxSubsystemCount:        ptr.To(int32(10)),
					VCPUCount:                ptr.To(int32(6)),
					EnableChecksumValidation: ptr.To(true),
					EnableAtomic4kWrites:     enableAtomic4kWrites,
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
	if err := simplyblockv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("adding the simplyblock scheme: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	apiClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}

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
			cluster := &simplyblockv1alpha1.StorageCluster{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "vcpumin-", Namespace: "default"},
				Spec: simplyblockv1alpha1.StorageClusterSpec{
					MaxSubsystemCount: ptr.To(int32(10)),
					VCPUCount:         ptr.To(tc.vcpuCount),
				},
			}

			err := apiClient.Create(context.Background(), cluster)
			if tc.wantDenied {
				if err == nil {
					t.Fatalf("expected the apiserver to reject vcpuCount %d, but it was accepted", tc.vcpuCount)
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
