// What an upgrade across 26.4.0-beta7 needs beyond the generic sequence, from
// the analysis of an upgrade from 26.4.0-beta6.2 (design-release-upgrade.md
// §6.6): a lblk reweight the target no longer finishes, the beta6 pNFS
// metadata-server state the target manages differently, the values the target
// chart no longer reads, and the switch to v2 write protection. One package per
// release keeps what a release needs in one place, and out of the generic
// sequence.

package r26_4_0_beta7

import (
	"context"
	"fmt"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/release"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/releases"
)

// The rules this module adds.
const (
	IDReweightPending       upgrade.ID = "r26-4-0-beta7-reweight-pending"
	IDHelmValuesRemovedKeys upgrade.ID = "r26-4-0-beta7-helm-values-removed-keys"
	IDPNFSMDSLegacyState    upgrade.ID = "r26-4-0-beta7-pnfs-mds-legacy-state"
	IDWriteProtectionV2     upgrade.ID = "r26-4-0-beta7-write-protection-v2"
)

// The beta6 metadata server's state disk: its claim template's name, and the
// suffix of the StorageClass and the admission policy that reserved it.
const (
	legacyStateClaim  = "state"
	legacyStateSuffix = "-pnfs-mds-state"
)

// Module is this release's module.
func Module(o releases.Options) releases.Module {
	return releases.Module{
		Release:       releases.MustVersion("26.4.0-beta7"),
		MinimumSource: releases.MustVersion("26.4.0-beta6"),
		Checks: []upgrade.Check{
			releases.NewCheck(IDReweightPending,
				"reminds that a lblk cluster must finish its pending reweight on the source release, which the target no longer completes",
				reweightPending),
			releases.NewCheck(IDHelmValuesRemovedKeys,
				"reports the pNFS metadata-server values the target chart no longer reads",
				helmValuesRemovedKeys),
		},
		Steps: map[releases.HookPoint][]upgrade.Step{
			// After the operator, because the beta6 operator would recreate the
			// StatefulSet with its claim template the moment it is deleted.
			releases.AfterOperator:     {pnfsMDSLegacyState{}},
			releases.AfterStoragePlane: {writeProtectionV2{o: o}},
		},
	}
}

// reweightPending is a warning, not a refusal: whether a reweight is pending is
// recorded in the control plane, which this tool does not read. A lblk cluster
// is the only kind that can have one.
func reweightPending(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
	var list simplyblockv1alpha2.StorageClusterList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, fmt.Errorf("list the storage clusters: %w", err)
	}
	finding := upgrade.Finding{
		Rule:     IDReweightPending,
		Severity: upgrade.SeverityWarning,
		Summary:  "a lblk cluster may have a device reweight pending, which 26.4.0-beta7 does not finish",
		Remediation: "on the source release, check reweight_pending with sbctl cluster get <cluster-id>, " +
			"and run sbctl cluster grow-devices until it is false before upgrading",
	}
	for i := range list.Items {
		if list.Items[i].Spec.DeviceClass == simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock {
			finding.Objects = append(finding.Objects, s.Ref(&list.Items[i]))
		}
	}
	if len(finding.Objects) == 0 {
		return nil, nil
	}
	return upgrade.Findings{finding}, nil
}

// helmValuesRemovedKeys warns about the values the target chart dropped. The
// chart keeps accepting them for one release, so they do not fail the upgrade,
// and they do nothing (design §7.4).
func helmValuesRemovedKeys(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
	deployed, found, err := release.Deployed(ctx, s.Client, s.Namespace)
	if err != nil || !found {
		return nil, err
	}
	mds := nested(deployed.Values, "driver", "pnfs", "mds")
	var keys []string
	for _, key := range []string{"stateSize", "stateStorageClassName"} {
		if _, ok := mds[key]; ok {
			keys = append(keys, "driver.pnfs.mds."+key)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	return upgrade.Findings{{
		Rule:        IDHelmValuesRemovedKeys,
		Severity:    upgrade.SeverityWarning,
		Summary:     "the release's values set " + strings.Join(keys, " and ") + ", which 26.4.0-beta7 no longer reads",
		Remediation: "remove them from the values file: the metadata server keeps its state on a volume of its own now",
	}}, nil
}

func nested(values map[string]any, path ...string) map[string]any {
	current := values
	for _, key := range path {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

// pnfsMDSLegacyState removes the beta6 metadata server's single-instance state:
// the StatefulSet that still carries the state claim template, its claims, the
// StorageClass derived for them, and the admission policy and binding that
// reserved that class. The target operator recreates the metadata server with
// its standby and a state volume it manages itself. NFS clients reconnect
// without reclaiming their state.
type pnfsMDSLegacyState struct{}

func (pnfsMDSLegacyState) ID() upgrade.ID         { return IDPNFSMDSLegacyState }
func (pnfsMDSLegacyState) Stage() upgrade.Stage   { return upgrade.StageReleaseRollout }
func (pnfsMDSLegacyState) Phase() upgrade.Phase   { return "" }
func (pnfsMDSLegacyState) Requires() []upgrade.ID { return nil }
func (pnfsMDSLegacyState) Description() string {
	return "removes the beta6 pNFS metadata server's state disk, so the target operator recreates it with its standby"
}

// legacy is everything the step removes, as it stands now.
type legacy struct {
	statefulSets []appsv1.StatefulSet
	claims       []corev1.PersistentVolumeClaim
	classes      []storagev1.StorageClass
	policies     []string
}

func (l legacy) empty() bool {
	return len(l.statefulSets)+len(l.claims)+len(l.classes)+len(l.policies) == 0
}

func (pnfsMDSLegacyState) read(ctx context.Context, s *upgrade.Scope) (legacy, error) {
	var out legacy
	var sets appsv1.StatefulSetList
	if err := s.Client.List(ctx, &sets, client.InNamespace(s.Namespace)); err != nil {
		return out, fmt.Errorf("list the StatefulSets: %w", err)
	}
	for _, set := range sets.Items {
		if !strings.Contains(set.Name, "-pnfs-mds-") {
			continue
		}
		for _, template := range set.Spec.VolumeClaimTemplates {
			if template.Name == legacyStateClaim {
				out.statefulSets = append(out.statefulSets, set)
				break
			}
		}
	}

	var claims corev1.PersistentVolumeClaimList
	if err := s.Client.List(ctx, &claims, client.InNamespace(s.Namespace)); err != nil {
		return out, fmt.Errorf("list the claims: %w", err)
	}
	for _, claim := range claims.Items {
		if claim.Spec.StorageClassName != nil && strings.HasSuffix(*claim.Spec.StorageClassName, legacyStateSuffix) {
			out.claims = append(out.claims, claim)
		}
	}

	var classes storagev1.StorageClassList
	if err := s.Client.List(ctx, &classes); err != nil {
		return out, fmt.Errorf("list the StorageClasses: %w", err)
	}
	for _, class := range classes.Items {
		if strings.HasSuffix(class.Name, legacyStateSuffix) {
			out.classes = append(out.classes, class)
		}
	}

	var policies admissionregistrationv1.ValidatingAdmissionPolicyList
	err := s.Client.List(ctx, &policies)
	if err != nil && !apierrors.IsNotFound(err) && !notServed(err) {
		return out, fmt.Errorf("list the admission policies: %w", err)
	}
	for _, policy := range policies.Items {
		if strings.HasSuffix(policy.Name, legacyStateSuffix) {
			out.policies = append(out.policies, policy.Name)
		}
	}
	return out, nil
}

// notServed is an error saying the kind is not served, which an API server
// without admission policies answers.
func notServed(err error) bool {
	return strings.Contains(err.Error(), "no matches for kind")
}

func (p pnfsMDSLegacyState) Describe(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	if !subject.IsUpgrade() {
		return nil, nil
	}
	found, err := p.read(ctx, s)
	if err != nil || found.empty() {
		return nil, err
	}
	var names []string
	for _, set := range found.statefulSets {
		names = append(names, "StatefulSet "+set.Name)
	}
	for _, claim := range found.claims {
		names = append(names, "PersistentVolumeClaim "+claim.Name)
	}
	for _, class := range found.classes {
		names = append(names, "StorageClass "+class.Name)
	}
	for _, policy := range found.policies {
		names = append(names, "ValidatingAdmissionPolicy and binding "+policy)
	}
	return &upgrade.Action{Rule: IDPNFSMDSLegacyState, Verb: upgrade.VerbDelete,
		Object: upgrade.ObjectRef{Namespace: s.Namespace, Name: "pnfs-mds-legacy-state"},
		Detail: strings.Join(names, ", ")}, nil
}

func (p pnfsMDSLegacyState) Done(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) (bool, error) {
	if !subject.IsUpgrade() {
		return false, nil
	}
	found, err := p.read(ctx, s)
	return err == nil && found.empty(), err
}

func (pnfsMDSLegacyState) Validate(context.Context, *upgrade.Scope, upgrade.Subject) error {
	return nil
}

func (p pnfsMDSLegacyState) Apply(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	if !subject.IsUpgrade() {
		return nil
	}
	found, err := p.read(ctx, s)
	if err != nil {
		return err
	}
	background := client.PropagationPolicy(metav1.DeletePropagationBackground)
	var objects []client.Object
	for i := range found.statefulSets {
		objects = append(objects, &found.statefulSets[i])
	}
	for i := range found.claims {
		objects = append(objects, &found.claims[i])
	}
	for i := range found.classes {
		objects = append(objects, &found.classes[i])
	}
	for _, name := range found.policies {
		objects = append(objects,
			&admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: name}},
			&admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	for _, obj := range objects {
		if err := s.Client.Delete(ctx, obj, background); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s: %w", obj.GetName(), err)
		}
	}
	return nil
}

func (p pnfsMDSLegacyState) Verify(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	done, err := p.Done(ctx, s, subject)
	if err != nil || !subject.IsUpgrade() || done {
		return err
	}
	return fmt.Errorf("the beta6 metadata-server state is still there after it was deleted")
}

// writeProtectionV2 switches every cluster still on v1 write protection, one
// cluster after the other. It runs after the storage plane, so every node runs
// the target's images before the switch reaches it.
type writeProtectionV2 struct{ o releases.Options }

func (writeProtectionV2) ID() upgrade.ID         { return IDWriteProtectionV2 }
func (writeProtectionV2) Stage() upgrade.Stage   { return upgrade.StageReleaseRollout }
func (writeProtectionV2) Phase() upgrade.Phase   { return "" }
func (writeProtectionV2) Requires() []upgrade.ID { return nil }
func (writeProtectionV2) Description() string {
	return "moves every cluster still on v1 write protection to v2 with a StorageClusterOps SwitchWriteProtection"
}

func (writeProtectionV2) onV1(ctx context.Context, s *upgrade.Scope) ([]string, error) {
	var list simplyblockv1alpha2.StorageClusterList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, fmt.Errorf("list the storage clusters: %w", err)
	}
	var out []string
	for _, cluster := range list.Items {
		if cluster.Status.WriteProtection == simplyblockv1alpha2.WriteProtectionV1 {
			out = append(out, cluster.Name)
		}
	}
	return out, nil
}

func (w writeProtectionV2) Describe(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	if !subject.IsUpgrade() {
		return nil, nil
	}
	clusters, err := w.onV1(ctx, s)
	if err != nil || len(clusters) == 0 {
		return nil, err
	}
	return &upgrade.Action{Rule: IDWriteProtectionV2, Verb: upgrade.VerbCreate,
		Object: upgrade.ObjectRef{Namespace: s.Namespace, Name: "write-protection"},
		Detail: "SwitchWriteProtection of " + strings.Join(clusters, ", ")}, nil
}

func (w writeProtectionV2) Done(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) (bool, error) {
	if !subject.IsUpgrade() {
		return false, nil
	}
	clusters, err := w.onV1(ctx, s)
	return err == nil && len(clusters) == 0, err
}

func (writeProtectionV2) Validate(context.Context, *upgrade.Scope, upgrade.Subject) error { return nil }

func (w writeProtectionV2) Apply(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	if !subject.IsUpgrade() {
		return nil
	}
	clusters, err := w.onV1(ctx, s)
	if err != nil {
		return err
	}
	target := releases.MustVersion("26.4.0-beta7")
	for _, cluster := range clusters {
		name := releases.OperationName(target, cluster, "wp")
		ops := &simplyblockv1alpha2.StorageClusterOps{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace},
			Spec: simplyblockv1alpha2.StorageClusterOpsSpec{
				ClusterRef: cluster,
				Action:     simplyblockv1alpha2.StorageClusterOpsActionSwitchWriteProtection,
			},
		}
		if err := s.Client.Create(ctx, ops); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s: %w", name, err)
		}
		if err := releases.AwaitClusterOps(ctx, s, w.o, name); err != nil {
			return err
		}
	}
	return nil
}

func (writeProtectionV2) Verify(context.Context, *upgrade.Scope, upgrade.Subject) error { return nil }
