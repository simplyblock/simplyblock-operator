// What a run's spec.discover.seed becomes in the draft: the cluster template and
// images it states, and the edge flag. It lives apart from the reconciler
// because the conversion is the one place the seed's all-optional shape meets
// the draft's types, and a seed that states nothing has to read as no seed.

package deployment

import (
	"strings"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// draftSeed splits a seed into the three things a draft carries. A member that
// states nothing comes back nil, so a partial seed leaves the rest to discovery.
func draftSeed(seed *simplyblockv1alpha2.DraftSeed) (
	*simplyblockv1alpha2.ClusterTemplate, *simplyblockv1alpha2.DeploymentImages, *bool,
) {
	if seed == nil {
		return nil, nil, nil
	}
	return seedCluster(seed.Cluster), seedImages(seed.Images), seed.EdgeCluster
}

func seedCluster(cluster *simplyblockv1alpha2.DraftSeedCluster) *simplyblockv1alpha2.ClusterTemplate {
	if cluster == nil {
		return nil
	}
	template := &simplyblockv1alpha2.ClusterTemplate{
		Name:                     strings.TrimSpace(cluster.Name),
		MaxSubsystemCount:        cluster.MaxSubsystemCount,
		EnableDriveFormat:        cluster.EnableDriveFormat,
		EnableChecksumValidation: cluster.EnableChecksumValidation,
		EnableAtomicity4K:        cluster.EnableAtomicity4K,
	}
	if cluster.Stripe != nil && (cluster.Stripe.DataChunks != nil || cluster.Stripe.ParityChunks != nil) {
		template.Stripe = cluster.Stripe.DeepCopy()
	}
	if template.Name == "" && template.Stripe == nil && template.MaxSubsystemCount == nil &&
		template.EnableDriveFormat == nil && template.EnableChecksumValidation == nil &&
		template.EnableAtomicity4K == nil {
		return nil
	}
	return template
}

func seedImages(images *simplyblockv1alpha2.DeploymentImages) *simplyblockv1alpha2.DeploymentImages {
	if images == nil || (images.SPDK == nil && images.SPDKProxy == nil && images.NodeAgent == nil) {
		return nil
	}
	return images.DeepCopy()
}
