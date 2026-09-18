// The approval guard: the edit that sets spec.approved is the last moment a
// deployment config can be corrected, so it is the one that is answered against
// the live cluster.
//
// The asymmetry between the two kinds of edit is the design. A draft is admitted
// on its structure alone, however wrong it is about the world, because a
// document that could not be saved until it was correct is a document nobody can
// work on; the controller reports what it found in status.message and the
// reviewer fixes it in place. The approving edit is checked, because §3.2 makes
// an approved document immutable: a config that reaches Failed on a misspelled
// worker cannot be corrected, only deleted and rewritten. Rejecting the
// approving apply costs one error message, and admitting it costs a dead
// document and a Failed record of a deployment that never happened.
//
// Devices are not checked here. Deciding whether one is mounted, busy, or
// partitioned runs per node and against the node rather than against the API
// server, which is discovery's job and then Validating's, the expansion's first
// step.
//
// Nothing is exempt, which is where this parts company with StorageNodeValidator
// next door. That one admits the operator's own service account because only the
// operator may re-point a node. Here the operator needs no exemption: a discovery
// run writes drafts, which is what the permissive path already admits, and a
// config the operator could approve and an administrator could not would be a
// gate with a hole in it.
//
// The §3.2 rules are restated below rather than left to the CEL on the type. The
// API server validates the schema before it calls a validating webhook, so for a
// current CRD those rejections are CEL's and arrive first; the restatement is
// what answers for a cluster whose CRD predates the rules, and it names the field
// that was edited and what to do instead where CEL names the rule that failed.
//
// Specified by operator/docs/designs/crd-redesign/design-clusterdeploymentconfig.md
// §5, whose five checks are checkApproval below.

package webhook

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/deployment"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-clusterdeploymentconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=clusterdeploymentconfigs,verbs=create;update,versions=v1alpha2,name=vclusterdeploymentconfig.simplyblock.io,admissionReviewVersions=v1

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=clusterdeploymentconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// ClusterDeploymentConfigValidator answers the approving edit against the
// Kubernetes API.
//
// failurePolicy=Fail, for the reason StorageNodeValidator already gives in this
// repository. The webhook server runs in the operator pod, so its availability
// tracks the operator's, and while the operator is down nothing expands anyway. A
// window in which approvals are admitted unchecked is a window in which immutable
// mistakes are created, and that is worse than a window in which no document can
// be approved.
type ClusterDeploymentConfigValidator struct {
	// Client reads the workers, the cluster, and the other configs the checks are
	// answered from. All of them are objects the operator already caches, which is
	// what makes them cheap enough to read inside an admission request.
	Client client.Client

	// Decoder turns the request's raw objects into documents.
	Decoder admission.Decoder
}

func (v *ClusterDeploymentConfigValidator) Handle(
	ctx context.Context, req admission.Request,
) admission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return admission.Allowed("")
	}

	config := &simplyblockv1alpha2.ClusterDeploymentConfig{}
	if err := v.Decoder.Decode(req, config); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	namespace := config.Namespace
	if namespace == "" {
		// A namespaced object created through a namespaced endpoint may arrive
		// with the field unset, because the path carries it instead.
		namespace = req.Namespace
	}

	if req.Operation == admissionv1.Update {
		old := &simplyblockv1alpha2.ClusterDeploymentConfig{}
		if err := v.Decoder.DecodeRaw(req.OldObject, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if old.Spec.Approved {
			return afterApproval(namespace, old, config)
		}
	}

	if !config.Spec.Approved {
		// A draft. The schema and the CEL rules of §3.2 are the whole check.
		return admission.Allowed("")
	}

	problems, err := v.checkApproval(ctx, namespace, config)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if len(problems) == 0 {
		return admission.Allowed("")
	}

	messages := make([]string, 0, len(problems))
	for _, problem := range problems {
		deployment.CountApprovalRejection(namespace, problem.reason)
		messages = append(messages, problem.message)
	}
	return admission.Denied(fmt.Sprintf(
		"this document cannot be approved, and approving it is what makes it "+
			"immutable: %s", strings.Join(messages, "; ")))
}

// problem is one thing wrong with an approval, carrying the reason it is counted
// under as well as the sentence the reviewer reads.
//
// The reason is the vocabulary the controller's own validation events use, for
// the five checks both perform, so the two counters of §9.2 can be read against
// each other: a rejection under a reason the draft never reported is a gap in
// the draft's validation rather than a reviewer's slip.
type problem struct {
	reason  string
	message string
}

// afterApproval restates §3.2 for a document that is already approved.
func afterApproval(
	namespace string, old, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) admission.Response {
	if !config.Spec.Approved {
		deployment.CountApprovalRejection(namespace, deployment.ApprovalWithdrawn)
		return admission.Denied(
			"spec.approved cannot be withdrawn: un-approving a document does not " +
				"un-expand it, and the cluster and nodes it produced are removed by " +
				"deleting them rather than by editing the record that describes them")
	}
	if !equality.Semantic.DeepEqual(old.Spec, config.Spec) {
		deployment.CountApprovalRejection(namespace, deployment.SpecImmutable)
		return admission.Denied(
			"spec is immutable once spec.approved is true, because the document is " +
				"then the record of what was deployed. To add nodes to the cluster " +
				"this one built, write a second config naming it in spec.clusterRef")
	}
	// Metadata alone. The operator labels an approved config itself, so this is
	// the ordinary edit rather than an exception to the rule above.
	return admission.Allowed("")
}

// checkApproval is §5.1's five checks, and returns everything wrong rather than
// the first thing wrong.
//
// A reviewer fixing a document that is about to become immutable wants the whole
// list: a document with a missing worker and a dangling cluster reference should
// say so once rather than over two applies, each of which costs another approval.
func (v *ClusterDeploymentConfigValidator) checkApproval(
	ctx context.Context, namespace string, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]problem, error) {
	var problems []problem

	missing, err := v.missingWorkers(ctx, config)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		problems = append(problems, problem{
			reason: deployment.WorkerNotFound,
			message: fmt.Sprintf(
				"a group names %s, which %s not %s of this Kubernetes cluster",
				strings.Join(missing, ", "),
				plural(len(missing), "is", "are"), plural(len(missing), "a node", "nodes")),
		})
	}

	// What the document says about erasure coding, against the deployment it
	// describes. It is the one check here that nothing downstream repeats: the
	// control plane validates the scheme on the cluster create, which lands after
	// this edit, and its activation gate counts devices rather than nodes.
	stripe, err := deployment.StripeChecks(ctx, v.Client, namespace, config)
	if err != nil {
		return nil, err
	}
	for _, check := range stripe {
		problems = append(problems, problem{reason: check.Reason, message: check.Message})
	}

	cluster, refused, err := v.resolveCluster(ctx, namespace, config)
	if err != nil {
		return nil, err
	}
	switch {
	case refused.reason != "":
		problems = append(problems, refused)

	case cluster != nil:
		// A growth document. The class the groups name has to be the one the
		// cluster is built out of: the nodes would otherwise be rejected one at a
		// time by StorageNodeValidator, which is a slower way to learn it and
		// leaves a half-expanded deployment behind.
		if mismatch := classMismatch(config, cluster); mismatch != "" {
			problems = append(problems, problem{
				reason: deployment.DeviceClassMismatch, message: mismatch,
			})
		}

	default:
		// A document that creates its cluster. No other approved config may
		// already own the one it would create.
		owner, err := v.otherOwner(ctx, namespace, config)
		if err != nil {
			return nil, err
		}
		if owner != "" {
			problems = append(problems, problem{
				reason: deployment.ClusterExists,
				message: fmt.Sprintf(
					"ClusterDeploymentConfig %s is approved and creates StorageCluster %s "+
						"as well; both would race to create it and the loser is an immutable "+
						"Failed document, so add to it with spec.clusterRef instead",
					owner, deployment.TargetClusterName(config)),
			})
		}
	}

	return problems, nil
}

// resolveCluster answers §6's table for the cluster the document names: the
// StorageCluster a growth document grows, nil for a document that may create its
// own, and a problem for the two combinations the expansion refuses.
func (v *ClusterDeploymentConfigValidator) resolveCluster(
	ctx context.Context, namespace string, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (*simplyblockv1alpha2.StorageCluster, problem, error) {
	name := deployment.TargetClusterName(config)
	if name == "" {
		return nil, problem{
			reason: deployment.NoClusterNamed,
			message: "the document names neither a cluster to create in spec.cluster " +
				"nor one to add nodes to in spec.clusterRef, so it describes no deployment",
		}, nil
	}

	var cluster simplyblockv1alpha2.StorageCluster
	err := v.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cluster)
	found := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, problem{}, fmt.Errorf("reading StorageCluster %s: %w", name, err)
	}

	switch {
	case config.Spec.ClusterRef != "" && !found:
		return nil, problem{
			reason: deployment.ClusterNotFound,
			message: fmt.Sprintf(
				"spec.clusterRef names StorageCluster %s, and there is none by that name "+
					"in namespace %s", name, namespace),
		}, nil

	case config.Spec.ClusterRef == "" && found:
		return nil, problem{
			reason: deployment.ClusterExists,
			message: fmt.Sprintf(
				"spec.cluster.name is %s and a StorageCluster by that name already exists; "+
					"set spec.clusterRef to add nodes to it instead", name),
		}, nil

	case found:
		return &cluster, problem{}, nil
	}
	return nil, problem{}, nil
}

// classMismatch reports a growth document whose groups name devices of a class
// its cluster is not built out of.
func classMismatch(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	cluster *simplyblockv1alpha2.StorageCluster,
) string {
	stated := deployment.DeviceClassOf(config)
	if stated == "" {
		// A document whose groups name no devices states no class, and there is
		// nothing to disagree with.
		return ""
	}

	held := cluster.Spec.DeviceClass
	if held == "" {
		// An unstated class is NVMe, which is what the cluster's own field
		// defaults to and what describes every cluster predating the field.
		held = simplyblockv1alpha2.StorageClusterDeviceClassNVMe
	}
	if stated == held {
		return ""
	}
	return fmt.Sprintf(
		"the groups name %s devices and StorageCluster %s is built out of %s; "+
			"an erasure-coding stripe placed across both classes is written at the "+
			"slower one's rate", stated, cluster.Name, held)
}

// missingWorkers names every worker the document lists that is not a node of this
// Kubernetes cluster.
func (v *ClusterDeploymentConfigValidator) missingWorkers(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]string, error) {
	var workers corev1.NodeList
	if err := v.Client.List(ctx, &workers); err != nil {
		return nil, fmt.Errorf("listing the Kubernetes workers: %w", err)
	}
	present := make(map[string]struct{}, len(workers.Items))
	for i := range workers.Items {
		present[workers.Items[i].Name] = struct{}{}
	}

	missing := map[string]struct{}{}
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				if _, there := present[worker]; !there {
					missing[worker] = struct{}{}
				}
			}
		}
	}

	named := make([]string, 0, len(missing))
	for worker := range missing {
		named = append(named, worker)
	}
	// Stable, so one document does not produce two differently ordered refusals.
	sort.Strings(named)
	return named, nil
}

// otherOwner names the approved config that already creates the cluster this one
// would create, where there is one.
//
// Only a document that creates a cluster owns it. Two growth documents adding
// nodes to one cluster is the design rather than a conflict: growth is a second
// document precisely so that the audit trail of how a cluster reached its size is
// a series of documents.
func (v *ClusterDeploymentConfigValidator) otherOwner(
	ctx context.Context, namespace string, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (string, error) {
	var configs simplyblockv1alpha2.ClusterDeploymentConfigList
	if err := v.Client.List(ctx, &configs, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("listing the deployment configs of namespace %s: %w", namespace, err)
	}

	wanted := deployment.TargetClusterName(config)
	for i := range configs.Items {
		other := &configs.Items[i]
		switch {
		case other.Name == config.Name:
			// This document, as the cache holds it.
		case !other.Spec.Approved:
			// A draft owns nothing. Whichever of the two is approved first
			// becomes the owner, and the other is refused then.
		case other.Spec.ClusterRef != "":
			// A growth document, which adds to a cluster rather than owning it.
		case deployment.TargetClusterName(other) == wanted:
			return other.Name, nil
		}
	}
	return "", nil
}
