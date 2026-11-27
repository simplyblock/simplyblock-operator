// The checks every release upgrade runs before it changes anything: the
// deployment is healthy, nothing is in flight, and it does not still store the
// v1alpha1 API (design-release-upgrade.md §6.5, step 1). A release module adds
// its own beside them. They live apart from the rollout's steps because they
// read and never write.

package releases

import (
	"context"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// The identities of the generic checks, which a report names and --skip takes.
const (
	IDClustersHealthy   upgrade.ID = "release-clusters-healthy"
	IDNodesOnline       upgrade.ID = "release-nodes-online"
	IDNothingInFlight   upgrade.ID = "release-nothing-in-flight"
	IDNoV1Alpha1Storage upgrade.ID = "release-no-v1alpha1-storage"
)

// prepareCheck is a check of the prepare stage, which is where every release
// check runs: before the upgrade has changed anything.
type prepareCheck struct {
	id          upgrade.ID
	description string
	check       func(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error)
}

func (c prepareCheck) ID() upgrade.ID          { return c.id }
func (c prepareCheck) Description() string     { return c.description }
func (c prepareCheck) Stages() []upgrade.Stage { return []upgrade.Stage{upgrade.StageReleasePrepare} }
func (c prepareCheck) Check(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
	return c.check(ctx, s)
}

// NewCheck is a check of the prepare stage, for a release module to declare.
func NewCheck(
	id upgrade.ID, description string,
	check func(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error),
) upgrade.Check {
	return prepareCheck{id: id, description: description, check: check}
}

// genericChecks are the checks every release upgrade runs.
func genericChecks() []upgrade.Check {
	return []upgrade.Check{
		NewCheck(IDClustersHealthy,
			"refuses an upgrade while a storage cluster is not online, because a cluster already absorbing a failure should not also absorb an upgrade",
			clustersHealthy),
		NewCheck(IDNodesOnline,
			"refuses an upgrade while a storage node is not online, because the rolling restart needs every peer online",
			nodesOnline),
		NewCheck(IDNothingInFlight,
			"refuses an upgrade while a cluster, node, or control-plane operation is running, because the upgrade restarts what it is driving",
			nothingInFlight),
		NewCheck(IDNoV1Alpha1Storage,
			"refuses an upgrade of a deployment that still stores the v1alpha1 API, which the API migration moves first",
			noV1Alpha1Storage),
	}
}

func clustersHealthy(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
	var list simplyblockv1alpha2.StorageClusterList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, fmt.Errorf("list the storage clusters: %w", err)
	}
	finding := upgrade.Finding{
		Rule:        IDClustersHealthy,
		Severity:    upgrade.SeverityError,
		Summary:     "a storage cluster is not online",
		Remediation: "let the cluster return to Online, or finish what it is doing, and run the upgrade again",
	}
	for i := range list.Items {
		cluster := &list.Items[i]
		if cluster.Status.Phase != simplyblockv1alpha2.StorageClusterPhaseOnline {
			finding.Objects = append(finding.Objects, s.Ref(cluster))
			finding.PerObject = append(finding.PerObject, fmt.Sprintf("phase %s", cluster.Status.Phase))
		}
	}
	return findingIfAny(finding), nil
}

func nodesOnline(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
	var list simplyblockv1alpha2.StorageNodeList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, fmt.Errorf("list the storage nodes: %w", err)
	}
	finding := upgrade.Finding{
		Rule:        IDNodesOnline,
		Severity:    upgrade.SeverityError,
		Summary:     "a storage node is not online",
		Remediation: "bring the node back online, or remove it, and run the upgrade again",
	}
	for i := range list.Items {
		node := &list.Items[i]
		switch node.Status.Phase {
		case simplyblockv1alpha2.StorageNodePhaseOnline, simplyblockv1alpha2.StorageNodePhaseRemoved:
			continue
		}
		finding.Objects = append(finding.Objects, s.Ref(node))
		finding.PerObject = append(finding.PerObject, fmt.Sprintf("phase %s", node.Status.Phase))
	}
	return findingIfAny(finding), nil
}

func nothingInFlight(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
	finding := upgrade.Finding{
		Rule:        IDNothingInFlight,
		Severity:    upgrade.SeverityError,
		Summary:     "an operation is running",
		Remediation: "wait for the operation to finish, or abort it, and run the upgrade again",
	}

	var clusterOps simplyblockv1alpha2.StorageClusterOpsList
	if err := s.Client.List(ctx, &clusterOps, client.InNamespace(s.Namespace)); err != nil {
		return nil, fmt.Errorf("list the cluster operations: %w", err)
	}
	for i := range clusterOps.Items {
		ops := &clusterOps.Items[i]
		if ops.Status.Phase == simplyblockv1alpha2.StorageClusterOpsPhasePending ||
			ops.Status.Phase == simplyblockv1alpha2.StorageClusterOpsPhaseRunning {
			finding.Objects = append(finding.Objects, s.Ref(ops))
			finding.PerObject = append(finding.PerObject, fmt.Sprintf("%s, %s", ops.Spec.Action, ops.Status.Phase))
		}
	}

	var nodeOps simplyblockv1alpha2.StorageNodeOpsList
	if err := s.Client.List(ctx, &nodeOps, client.InNamespace(s.Namespace)); err != nil {
		return nil, fmt.Errorf("list the node operations: %w", err)
	}
	for i := range nodeOps.Items {
		ops := &nodeOps.Items[i]
		if ops.Status.Phase == simplyblockv1alpha2.StorageNodeOpsPhasePending ||
			ops.Status.Phase == simplyblockv1alpha2.StorageNodeOpsPhaseRunning {
			finding.Objects = append(finding.Objects, s.Ref(ops))
			finding.PerObject = append(finding.PerObject, fmt.Sprintf("%s, %s", ops.Spec.Action, ops.Status.Phase))
		}
	}

	var cpOps simplyblockv1alpha2.ControlPlaneOpsList
	if err := s.Client.List(ctx, &cpOps, client.InNamespace(s.Namespace)); err != nil {
		return nil, fmt.Errorf("list the control-plane operations: %w", err)
	}
	for i := range cpOps.Items {
		ops := &cpOps.Items[i]
		if ops.Status.Phase == simplyblockv1alpha2.ControlPlaneOpsPhasePending ||
			ops.Status.Phase == simplyblockv1alpha2.ControlPlaneOpsPhaseRunning {
			finding.Objects = append(finding.Objects, s.Ref(ops))
			finding.PerObject = append(finding.PerObject, fmt.Sprintf("%s, %s", ops.Spec.Action, ops.Status.Phase))
		}
	}
	return findingIfAny(finding), nil
}

// noV1Alpha1Storage reads the storage version off the StorageCluster CRD, the
// kind every deployment has. A deployment storing v1alpha1 is one the API
// migration has not reached, and every release module assumes v1alpha2.
func noV1Alpha1Storage(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
	var crd apiextensionsv1.CustomResourceDefinition
	err := s.Client.Get(ctx, client.ObjectKey{Name: "storageclusters.storage.simplyblock.io"}, &crd)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the StorageCluster CRD: %w", err)
	}
	for _, version := range crd.Spec.Versions {
		if version.Storage && version.Name == "v1alpha1" {
			return upgrade.Findings{{
				Rule:        IDNoV1Alpha1Storage,
				Severity:    upgrade.SeverityError,
				Summary:     "the deployment still stores the v1alpha1 API",
				Remediation: "run simplyblock-upgrade upgrade and migrate first, then the release upgrade",
			}}, nil
		}
	}
	return nil, nil
}

func findingIfAny(f upgrade.Finding) upgrade.Findings {
	if len(f.Objects) == 0 {
		return nil
	}
	return upgrade.Findings{f}
}
