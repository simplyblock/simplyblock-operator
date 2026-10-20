package volumemigration

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestGetConfig(t *testing.T) {
	// Regression: 2026-10-20-job-image-main-fallback — the latency benchmark and
	// the injected rebalancer fell back to simplyblock-rebalancer:latest, a
	// release other than the operator's own.
	t.Run("nil settings yield the operator's release", func(t *testing.T) {
		t.Setenv(operatorImageEnv, releaseOperatorImage)
		t.Setenv(RebalancerImageEnvVar, "")
		cfg := GetConfig(nil)
		if got := ptr.From(cfg.RebalancerImage, ""); got != releaseRebalancerImage {
			t.Errorf("RebalancerImage = %q, want %q", got, releaseRebalancerImage)
		}
	})

	// Nothing to derive it from leaves the image unset, which both callers read
	// as the rebalancer not being configured.
	t.Run("nil settings with nothing naming an image leave it unset", func(t *testing.T) {
		t.Setenv(operatorImageEnv, "")
		t.Setenv(RebalancerImageEnvVar, "")
		cfg := GetConfig(nil)
		if got := ptr.From(cfg.RebalancerImage, ""); got != "" {
			t.Errorf("RebalancerImage = %q, want it unset", got)
		}
	})

	t.Run("nil settings pick up the environment override", func(t *testing.T) {
		t.Setenv(RebalancerImageEnvVar, "registry.example.com/rebalancer:v9")
		cfg := GetConfig(nil)
		if cfg.RebalancerImage == nil || *cfg.RebalancerImage != "registry.example.com/rebalancer:v9" {
			t.Errorf("RebalancerImage = %v, want the override", cfg.RebalancerImage)
		}
	})

	// Regression: 2026-10-20-job-image-main-fallback — a cluster that states
	// migration settings without an image got no image at all, so the same
	// cluster ran different rebalancers depending on whether the block existed.
	t.Run("settings without an image yield the operator's release and keep the rest", func(t *testing.T) {
		t.Setenv(operatorImageEnv, releaseOperatorImage)
		t.Setenv(RebalancerImageEnvVar, "")
		spec := &simplyblockv1alpha2.VolumeMigrationSettings{
			DataRealignment: &simplyblockv1alpha2.DataRealignmentSettings{MinMoves: ptr.To(int32(4))},
		}
		cfg := GetConfig(spec)
		if got := ptr.From(cfg.RebalancerImage, ""); got != releaseRebalancerImage {
			t.Errorf("RebalancerImage = %q, want %q", got, releaseRebalancerImage)
		}
		if cfg.DataRealignment == nil || ptr.From(cfg.DataRealignment.MinMoves, 0) != 4 {
			t.Errorf("DataRealignment = %+v, want the minMoves it asked for", cfg.DataRealignment)
		}
		if spec.RebalancerImage != nil {
			t.Errorf("the cluster's own settings were written to: RebalancerImage = %q", *spec.RebalancerImage)
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
