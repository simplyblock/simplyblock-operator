package volumemigration

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestDefaultRebalancerImage(t *testing.T) {
	t.Run("falls back to the pinned default", func(t *testing.T) {
		t.Setenv(RebalancerImageEnvVar, "")
		if got := defaultRebalancerImage(); got != DefaultRebalancerImage {
			t.Errorf("image = %q, want %q", got, DefaultRebalancerImage)
		}
	})

	// The Helm chart sets this to the operator's own tag so the rebalancer image and
	// the operator stay in lockstep; honouring it is what keeps them from drifting.
	t.Run("environment override wins", func(t *testing.T) {
		t.Setenv(RebalancerImageEnvVar, "registry.example.com/rebalancer:v9")
		if got := defaultRebalancerImage(); got != "registry.example.com/rebalancer:v9" {
			t.Errorf("image = %q, want the override", got)
		}
	})
}

func TestGetConfig(t *testing.T) {
	t.Run("nil settings yield the default image", func(t *testing.T) {
		t.Setenv(RebalancerImageEnvVar, "")
		cfg := GetConfig(nil)
		if cfg.RebalancerImage == nil || *cfg.RebalancerImage != DefaultRebalancerImage {
			t.Errorf("RebalancerImage = %v, want the default", cfg.RebalancerImage)
		}
	})

	t.Run("nil settings pick up the environment override", func(t *testing.T) {
		t.Setenv(RebalancerImageEnvVar, "registry.example.com/rebalancer:v9")
		cfg := GetConfig(nil)
		if cfg.RebalancerImage == nil || *cfg.RebalancerImage != "registry.example.com/rebalancer:v9" {
			t.Errorf("RebalancerImage = %v, want the override", cfg.RebalancerImage)
		}
	})

	// A provided spec is returned as-is: the default must not overwrite what the
	// StorageCluster asked for.
	t.Run("provided settings are returned unchanged", func(t *testing.T) {
		t.Setenv(RebalancerImageEnvVar, "registry.example.com/ignored:v9")
		spec := &simplyblockv1alpha2.VolumeMigrationSettings{
			RebalancerImage: ptr.To("pinned:v1"),
			DataRealignment: &simplyblockv1alpha2.DataRealignmentSettings{MinMoves: ptr.To(int32(4))},
		}
		cfg := GetConfig(spec)
		if cfg.RebalancerImage == nil || *cfg.RebalancerImage != "pinned:v1" {
			t.Errorf("RebalancerImage = %v, want pinned:v1", cfg.RebalancerImage)
		}
		if cfg.DataRealignment == nil || ptr.From(cfg.DataRealignment.MinMoves, 0) != 4 {
			t.Errorf("DataRealignment = %+v, want the minMoves it asked for", cfg.DataRealignment)
		}
	})
}
