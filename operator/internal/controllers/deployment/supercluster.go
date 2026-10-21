// A document that describes a supercluster (design-superclusters.md §5): two or
// more node sets and no cluster to grow, each node set expanding into a member
// cluster of a supercluster named spec.cluster.name. This file holds what tells
// such a document apart, the per-member view of it every per-cluster check and
// builder reads, and how a member is named.

package deployment

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// IsSuperclusterDocument reports whether the document creates a supercluster
// rather than one cluster. A document with one node set reads as it always
// has, and growth stays a document naming its cluster in spec.clusterRef.
func IsSuperclusterDocument(config *simplyblockv1alpha2.ClusterDeploymentConfig) bool {
	return config.Spec.ClusterRef == "" && config.Spec.Cluster != nil && len(config.Spec.NodeSets) > 1
}

// clustersBuilt is how many clusters the document builds nodes for: each
// member of a supercluster, or the one cluster.
func clustersBuilt(config *simplyblockv1alpha2.ClusterDeploymentConfig) int {
	if IsSuperclusterDocument(config) {
		return len(config.Spec.NodeSets)
	}
	return 1
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

// expansionTarget is one cluster the expansion builds nodes for, and the part of
// the document that describes it.
type expansionTarget struct {
	cluster  string
	document *simplyblockv1alpha2.ClusterDeploymentConfig
}

// expansionTargets are the clusters the document builds nodes for: the cluster
// it created or grows, or each member of the supercluster it created, as
// status.members records them. A supercluster's document whose record misses a
// node set is refused rather than read as one with fewer members, since every
// step after CreatingCluster would otherwise pass with nothing to do.
func expansionTargets(config *simplyblockv1alpha2.ClusterDeploymentConfig) ([]expansionTarget, error) {
	if !IsSuperclusterDocument(config) {
		return []expansionTarget{{cluster: config.Status.ClusterRef, document: config}}, nil
	}
	recorded := map[string]string{}
	for _, entry := range config.Status.Members {
		recorded[entry.NodeSet] = entry.Cluster
	}
	targets := make([]expansionTarget, 0, len(config.Spec.NodeSets))
	for _, set := range config.Spec.NodeSets {
		cluster, ok := recorded[set.Name]
		if !ok {
			return nil, fmt.Errorf("status.members records no member for node set %s", set.Name)
		}
		targets = append(targets, expansionTarget{cluster: cluster, document: memberView(config, set)})
	}
	return targets, nil
}

// createSupercluster is CreatingCluster for a supercluster's document (§5.4):
// the supercluster first, because a member names it in a field admission
// resolves; then every member's name, recorded before any member is created;
// then the members, by the recorded names.
func (r *ClusterDeploymentConfigReconciler) createSupercluster(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (bool, error) {
	name := config.Spec.Cluster.Name
	var existing simplyblockv1alpha2.StorageCluster
	switch err := r.Get(ctx, client.ObjectKey{Namespace: config.Namespace, Name: name}, &existing); {
	case err == nil && config.Status.ClusterRef != name:
		return false, refusef(ClusterExists,
			"spec.cluster.name is %s and a StorageCluster by that name already exists", name)
	case apierrors.IsNotFound(err):
		supercluster := &simplyblockv1alpha2.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Namespace},
			Spec: simplyblockv1alpha2.StorageClusterSpec{
				Supercluster: &simplyblockv1alpha2.SuperclusterSpec{},
			},
		}
		if err := r.Create(ctx, supercluster); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("creating supercluster %s: %w", name, err)
		}
		r.emit(config, corev1.EventTypeNormal, ClusterCreated, fmt.Sprintf("Created supercluster %s", name))
	case err != nil:
		return false, fmt.Errorf("reading StorageCluster %s: %w", name, err)
	}
	if config.Status.ClusterRef != name {
		if err := r.recordCluster(ctx, config, name); err != nil {
			return false, err
		}
		config.Status.ClusterRef = name
	}

	members, err := r.recordMembers(ctx, config)
	if err != nil {
		return false, err
	}

	for _, entry := range members {
		for _, set := range config.Spec.NodeSets {
			if set.Name != entry.NodeSet {
				continue
			}
			member, err := r.buildCluster(memberView(config, set), entry.Cluster)
			if err != nil {
				return false, err
			}
			member.Spec.SuperclusterRef = name
			if err := r.Create(ctx, member); err != nil {
				if apierrors.IsAlreadyExists(err) {
					continue
				}
				return false, fmt.Errorf("creating member %s: %w", entry.Cluster, err)
			}
			r.emit(config, corev1.EventTypeNormal, ClusterCreated, fmt.Sprintf(
				"Created member %s of supercluster %s from node set %s", entry.Cluster, name, set.Name))
		}
	}
	return true, nil
}

// recordMembers draws a name for every node set that has none recorded and
// writes them all before any member is created, so a crash between two
// creates resumes with the same names. A drawn name that is already a
// StorageCluster, or a suffix another member carries, is drawn again, because
// the suffix tells members apart in node hostnames (§5.3, §6.2).
func (r *ClusterDeploymentConfigReconciler) recordMembers(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]simplyblockv1alpha2.ExpansionMember, error) {
	members := append([]simplyblockv1alpha2.ExpansionMember(nil), config.Status.Members...)
	recorded := map[string]bool{}
	suffixes := map[string]bool{}
	for _, entry := range members {
		recorded[entry.NodeSet] = true
		suffixes[entry.Cluster[len(entry.Cluster)-memberSuffixLength:]] = true
	}

	drawn := false
	for _, set := range config.Spec.NodeSets {
		if recorded[set.Name] {
			continue
		}
		for {
			suffix, err := drawMemberSuffix()
			if err != nil {
				return nil, err
			}
			candidate := memberName(config.Spec.Cluster.Name, set.Name, suffix)
			taken, err := r.clusterExists(ctx, config.Namespace, candidate)
			if err != nil {
				return nil, err
			}
			if taken || suffixes[suffix] {
				continue
			}
			suffixes[suffix] = true
			members = append(members, simplyblockv1alpha2.ExpansionMember{NodeSet: set.Name, Cluster: candidate})
			drawn = true
			break
		}
	}
	if !drawn {
		return members, nil
	}
	err := r.writeStatus(ctx, config, func(status *simplyblockv1alpha2.ClusterDeploymentConfigStatus) {
		status.Members = members
	})
	if err != nil {
		return nil, err
	}
	// The write reports back what the apiserver stored. An apiserver whose
	// schema predates status.members drops the field without an error, and a
	// member created then would be drawn again on the next pass.
	if len(config.Status.Members) != len(members) {
		return nil, fmt.Errorf("the apiserver did not keep status.members, so no member is created " +
			"under a name that is not on record: the ClusterDeploymentConfig CRD predates the field " +
			"and has to be updated")
	}
	return members, nil
}

// clusterExists reports whether a StorageCluster of the name exists.
func (r *ClusterDeploymentConfigReconciler) clusterExists(ctx context.Context, namespace, name string) (bool, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cluster)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
