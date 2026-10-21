// A document that describes a supercluster (design-superclusters.md §5): two or
// more node sets and no cluster to grow, each node set expanding into a member
// cluster of a supercluster named spec.cluster.name. This file holds what tells
// such a document apart, the per-member view of it every per-cluster check and
// builder reads, and how a member is named.

package deployment

import (
	"crypto/rand"
	"fmt"
	"math/big"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// IsSuperclusterDocument reports whether the document creates a supercluster
// rather than one cluster. A document with one node set reads as it always
// has, and growth stays a document naming its cluster in spec.clusterRef.
func IsSuperclusterDocument(config *simplyblockv1alpha2.ClusterDeploymentConfig) bool {
	return config.Spec.ClusterRef == "" && config.Spec.Cluster != nil && len(config.Spec.NodeSets) > 1
}

// memberView is the document as one member reads it: the same template and
// images, and one node set. Every check and builder that spans a cluster runs
// on it unchanged, so what spans a cluster spans a member.
func memberView(
	config *simplyblockv1alpha2.ClusterDeploymentConfig, set simplyblockv1alpha2.NodeSet,
) *simplyblockv1alpha2.ClusterDeploymentConfig {
	view := config.DeepCopy()
	view.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{*set.DeepCopy()}
	return view
}

// memberName is <cluster>-<nodeset>-<suffix> (§5.3).
func memberName(cluster, nodeSet, suffix string) string {
	return fmt.Sprintf("%s-%s-%s", cluster, nodeSet, suffix)
}

// memberSuffixLetters is the alphabet of a member's suffix, and its length.
const (
	memberSuffixLetters = "abcdefghijklmnopqrstuvwxyz"
	memberSuffixLength  = 3
)

// drawMemberSuffix draws three random lowercase letters. A member's name
// carries no position, so a removed member's name is never reused by the next
// member from the same node set name (§5.3).
func drawMemberSuffix() (string, error) {
	suffix := make([]byte, memberSuffixLength)
	for i := range suffix {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(memberSuffixLetters))))
		if err != nil {
			return "", fmt.Errorf("draw a member suffix: %w", err)
		}
		suffix[i] = memberSuffixLetters[n.Int64()]
	}
	return string(suffix), nil
}
