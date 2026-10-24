// A document whose node sets become member clusters (design-superclusters.md
// §5): one that creates a supercluster, with two or more node sets and no
// cluster to grow, and one that adds members to a supercluster it names in
// spec.clusterRef (§5.6). This file holds what tells such a document apart, the
// per-member view of it every per-cluster check and builder reads, and how a
// member is named.

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

// AddsMembers reports whether each node set of the document becomes a member
// cluster: a document creating a supercluster, or one naming a supercluster in
// spec.clusterRef. A clusterRef that names nothing is not a supercluster, and
// the expansion refuses it with ClusterNotFound.
func AddsMembers(
	ctx context.Context, reader client.Reader, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (bool, error) {
	if IsSuperclusterDocument(config) {
		return true, nil
	}
	if config.Spec.ClusterRef == "" {
		return false, nil
	}
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: config.Namespace, Name: config.Spec.ClusterRef}
	switch err := reader.Get(ctx, key, &cluster); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading StorageCluster %s: %w", config.Spec.ClusterRef, err)
	}
	return cluster.Spec.Supercluster != nil, nil
}

// superclusterName is the supercluster a members document creates or adds to.
func superclusterName(config *simplyblockv1alpha2.ClusterDeploymentConfig) string {
	if config.Spec.ClusterRef != "" {
		return config.Spec.ClusterRef
	}
	if config.Spec.Cluster == nil {
		return ""
	}
	return config.Spec.Cluster.Name
}

// asCreation is a document adding members as one creating them, under the
// supercluster's name: each node set is a new cluster built from the template,
// so every check that reads clusterRef for the cluster a document grows reads
// the template instead.
func asCreation(config *simplyblockv1alpha2.ClusterDeploymentConfig) *simplyblockv1alpha2.ClusterDeploymentConfig {
	if config.Spec.ClusterRef == "" {
		return config
	}
	creation := config.DeepCopy()
	creation.Spec.ClusterRef = ""
	if creation.Spec.Cluster != nil {
		creation.Spec.Cluster.Name = config.Spec.ClusterRef
	}
	return creation
}

// clustersBuilt is how many clusters the document builds nodes for: each
// member, or the one cluster.
func clustersBuilt(config *simplyblockv1alpha2.ClusterDeploymentConfig, members bool) int {
	if members {
		return len(config.Spec.NodeSets)
	}
	return 1
}

// memberView is the document as one member reads it: the same images, one node
// set, and the template with that node set's overrides merged in (§5.8). Every
// check and builder that spans a cluster runs on it unchanged, so what spans a
// cluster spans a member, at the member's own sizing. A member is a new cluster
// whichever document adds it, so the view names no cluster to grow.
func memberView(
	config *simplyblockv1alpha2.ClusterDeploymentConfig, set simplyblockv1alpha2.NodeSet,
) *simplyblockv1alpha2.ClusterDeploymentConfig {
	view := asCreation(config).DeepCopy()
	view.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{*set.DeepCopy()}
	if template, overrides := view.Spec.Cluster, set.Cluster; template != nil && overrides != nil {
		applyOverrides(template, overrides.DeepCopy())
	}
	return view
}

// applyOverrides writes every field the node set states over the template's.
func applyOverrides(
	template *simplyblockv1alpha2.ClusterTemplate, overrides *simplyblockv1alpha2.NodeSetClusterOverrides,
) {
	if overrides.VCPUCount != nil {
		template.VCPUCount = overrides.VCPUCount
	}
	if overrides.MaxSubsystemCount != nil {
		template.MaxSubsystemCount = overrides.MaxSubsystemCount
	}
	if overrides.MinHugePagesSize != "" {
		template.MinHugePagesSize = overrides.MinHugePagesSize
	}
	if len(overrides.SocketsToUse) > 0 {
		template.SocketsToUse = overrides.SocketsToUse
	}
	if overrides.NodesPerSocket != nil {
		template.NodesPerSocket = overrides.NodesPerSocket
	}
	if overrides.Stripe != nil {
		template.Stripe = overrides.Stripe
	}
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

// drawSuffix draws a member's suffix. It is a variable so that a test can
// draw a suffix another member already carries.
var drawSuffix = drawMemberSuffix

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
// it created or grows, or each member it created, as status.members records
// them. A members document whose record misses a node set is refused rather
// than read as one with fewer members, since every step after CreatingCluster
// would otherwise pass with nothing to do.
func (r *ClusterDeploymentConfigReconciler) expansionTargets(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]expansionTarget, error) {
	members, err := AddsMembers(ctx, r.Client, config)
	if err != nil {
		return nil, err
	}
	if !members {
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

// createSupercluster is CreatingCluster for a members document (§5.4, §5.6):
// the supercluster first where the document creates it, because a member names
// it in a field admission resolves; then every member's name, recorded before
// any member is created; then the members, by the recorded names.
func (r *ClusterDeploymentConfigReconciler) createSupercluster(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (bool, error) {
	name := superclusterName(config)
	if config.Spec.ClusterRef == "" {
		if err := r.ensureSupercluster(ctx, config, name); err != nil {
			return false, err
		}
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

// ensureSupercluster creates the supercluster a document creates, and refuses
// a name another StorageCluster already has.
func (r *ClusterDeploymentConfigReconciler) ensureSupercluster(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig, name string,
) error {
	var existing simplyblockv1alpha2.StorageCluster
	switch err := r.Get(ctx, client.ObjectKey{Namespace: config.Namespace, Name: name}, &existing); {
	case err == nil && config.Status.ClusterRef != name:
		return refusef(ClusterExists,
			"spec.cluster.name is %s and a StorageCluster by that name already exists", name)
	case apierrors.IsNotFound(err):
		supercluster := &simplyblockv1alpha2.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Namespace},
			Spec: simplyblockv1alpha2.StorageClusterSpec{
				Supercluster: &simplyblockv1alpha2.SuperclusterSpec{},
			},
		}
		if err := r.Create(ctx, supercluster); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating supercluster %s: %w", name, err)
		}
		r.emit(config, corev1.EventTypeNormal, ClusterCreated, fmt.Sprintf("Created supercluster %s", name))
	case err != nil:
		return fmt.Errorf("reading StorageCluster %s: %w", name, err)
	}
	return nil
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
	suffixes, err := r.memberSuffixes(ctx, config.Namespace, superclusterName(config))
	if err != nil {
		return nil, err
	}
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
			suffix, err := drawSuffix()
			if err != nil {
				return nil, err
			}
			candidate := memberName(superclusterName(config), set.Name, suffix)
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
	err = r.writeStatus(ctx, config, func(status *simplyblockv1alpha2.ClusterDeploymentConfigStatus) {
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

// memberSuffixes are the suffixes the supercluster's members already carry,
// including members another document added.
func (r *ClusterDeploymentConfigReconciler) memberSuffixes(
	ctx context.Context, namespace, supercluster string,
) (map[string]bool, error) {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing the members of supercluster %s: %w", supercluster, err)
	}
	suffixes := map[string]bool{}
	for _, cluster := range clusters.Items {
		if cluster.Spec.SuperclusterRef == supercluster && len(cluster.Name) > memberSuffixLength {
			suffixes[cluster.Name[len(cluster.Name)-memberSuffixLength:]] = true
		}
	}
	return suffixes, nil
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
