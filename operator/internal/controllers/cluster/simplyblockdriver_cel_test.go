// Validation of the CEL rules on SimplyblockDriver's pNFS data network, run
// against a real apiserver. It lives beside the StorageCluster CEL tests
// because this package's suite is the one that starts an apiserver with the
// CRDs alone, so a rejection here can only have come from the schema.

package cluster

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// TestSimplyblockDriverDataNetworkRules covers the address range the metadata
// server's data-network attachment allocates from: an IPv4 subnet, and a first
// and last address inside it.
func TestSimplyblockDriverDataNetworkRules(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name       string
		network    simplyblockv1alpha2.DriverPNFSDataNetwork
		wantDenied string
	}{
		{
			name:    "a range inside its subnet",
			network: simplyblockv1alpha2.DriverPNFSDataNetwork{Subnet: "10.10.10.0/24", RangeStart: "10.10.10.200", RangeEnd: "10.10.10.209"},
		},
		{
			name:       "a subnet that is not a CIDR",
			network:    simplyblockv1alpha2.DriverPNFSDataNetwork{Subnet: "10.10.10.0", RangeStart: "10.10.10.200", RangeEnd: "10.10.10.209"},
			wantDenied: "subnet must be an IPv4 CIDR",
		},
		{
			name:       "an IPv6 subnet",
			network:    simplyblockv1alpha2.DriverPNFSDataNetwork{Subnet: "fd00::/64", RangeStart: "fd00::10", RangeEnd: "fd00::20"},
			wantDenied: "subnet must be an IPv4 CIDR",
		},
		{
			name:       "a first address outside the subnet",
			network:    simplyblockv1alpha2.DriverPNFSDataNetwork{Subnet: "10.10.10.0/24", RangeStart: "10.10.11.200", RangeEnd: "10.10.10.209"},
			wantDenied: "rangeStart must be an address in subnet",
		},
		{
			name:       "a last address outside the subnet",
			network:    simplyblockv1alpha2.DriverPNFSDataNetwork{Subnet: "10.10.10.0/24", RangeStart: "10.10.10.200", RangeEnd: "10.10.11.209"},
			wantDenied: "rangeEnd must be an address in subnet",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			network := tc.network
			driver := &simplyblockv1alpha2.SimplyblockDriver{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-", Namespace: "default"},
				Spec: simplyblockv1alpha2.SimplyblockDriverSpec{
					PNFS: simplyblockv1alpha2.DriverPNFS{
						MDS: &simplyblockv1alpha2.DriverPNFSMDS{DataNetwork: &network},
					},
				},
			}

			err := apiClient.Create(context.Background(), driver)
			if tc.wantDenied != "" {
				if err == nil {
					t.Fatal("expected the apiserver to reject the driver, but it was accepted")
				}
				if !strings.Contains(err.Error(), tc.wantDenied) {
					t.Fatalf("rejected for the wrong reason, want %q: %v", tc.wantDenied, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected the apiserver to accept the driver, got: %v", err)
			}
			if err := apiClient.Delete(context.Background(), driver); err != nil {
				t.Fatalf("deleting the driver: %v", err)
			}
		})
	}
}
