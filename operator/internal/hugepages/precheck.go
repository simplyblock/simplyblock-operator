// The Preprovisioned huge-page precheck: whether a candidate worker already
// carries enough hugepages-2Mi capacity for a cluster's sizing, and whether
// that capacity is actually free rather than held by something else.
//
// Specified by design-preprovisioned-hugepages.md S:5. Reached only when a
// document's or node's HugePagesMode is Preprovisioned (same design, S:6).

package hugepages

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/discovery"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// twoMiBBytes is the hugepages-2Mi page size, the only size this precheck
	// compares against: it is the size storage_deploy_spdk.yaml.j2 requests.
	twoMiBBytes = int64(2 * 1024 * 1024)

	// mibBytes converts a MiB figure (the control plane's and this package's
	// own unit throughout) to bytes (KubeNode's and corev1.Quantity's).
	mibBytes = int64(1024 * 1024)

	// hugePages2MiResource is the resource name both the kubelet and an SPDK
	// pod's own resources.limits name this page size by.
	hugePages2MiResource = corev1.ResourceName("hugepages-2Mi")

	// clusterLabel is the label storage_deploy_spdk.yaml.j2 puts on an SPDK
	// pod with the control plane's full cluster id (the same label
	// internal/controllers/node/network_podnetwork.go reads for the same
	// pod, under its own unexported copy of this constant: there is no
	// shared label-constants package yet for either side to depend on).
	clusterLabel = "simplyblock.io/cluster"

	// ReasonInsufficientHugePages is set when a worker's hugepages-2Mi
	// capacity, were nothing else consuming it, is already below what the
	// cluster's sizing requires.
	ReasonInsufficientHugePages = "InsufficientHugePages"

	// ReasonHugePagesContended is set when a worker's capacity would be
	// enough on its own, but other pods already hold enough of it that the
	// remainder is not.
	ReasonHugePagesContended = "HugePagesContended"

	// ReasonReady is set when every candidate worker passed.
	ReasonReady = "Ready"

	// ConditionHugePagesReady is the metav1.Condition.Type a controller's own
	// call site writes onto its document or cluster (design S:4.2, S:6.1). The
	// StorageNode admission site denies the request directly and writes no
	// condition.
	ConditionHugePagesReady = "HugePagesReady"
)

// EstimateHugePagesClient is the one control-plane capability Evaluate needs:
// the subset of *controlplane.Client that lets a unit test stub the sizing
// formula's result without a server. *controlplane.Client implements this
// structurally.
type EstimateHugePagesClient interface {
	EstimateHugePages(ctx context.Context, maxSubsys, vcpuCount int32) (mib int64, err error)
}

// WorkerResult is one candidate worker's outcome.
type WorkerResult struct {
	// Name is the worker's node name.
	Name string
	// Ready is whether the worker passed both steps.
	Ready bool
	// Reason is one of the Reason constants, empty when Ready.
	Reason string
	// Message is one sentence naming the figures or the pods involved, for an
	// Event or an admission denial to quote directly.
	Message string
}

// Result is the precheck's outcome across every worker a document or a node
// named.
type Result struct {
	// RequiredMiB is the figure every worker was compared against: the
	// control plane's own sizing formula, floored and rounded exactly as the
	// real pod's hugepages-2Mi limit will be (S:5 Step 1).
	RequiredMiB int64
	// Workers is one entry per candidate worker, in the order Evaluate was
	// given them.
	Workers []WorkerResult
}

// AllReady is whether every worker passed, vacuously true for zero workers:
// an empty candidate set has nothing to fail (U-14).
func (r *Result) AllReady() bool {
	for _, w := range r.Workers {
		if !w.Ready {
			return false
		}
	}
	return true
}

// Condition turns this Result into the HugePagesReady condition a controller's
// own call site writes (design S:4.2): True with reason Ready once every
// worker passed, False with the first failing worker's reason and message
// otherwise. ObservedGeneration is the caller's to stamp.
func (r *Result) Condition() metav1.Condition {
	for _, w := range r.Workers {
		if !w.Ready {
			return metav1.Condition{
				Type: ConditionHugePagesReady, Status: metav1.ConditionFalse,
				Reason: w.Reason, Message: w.Message,
			}
		}
	}
	return metav1.Condition{
		Type: ConditionHugePagesReady, Status: metav1.ConditionTrue,
		Reason: ReasonReady, Message: "every candidate worker carries enough free hugepages-2Mi",
	}
}

func (r *Result) pass(name string) {
	r.Workers = append(r.Workers, WorkerResult{Name: name, Ready: true})
}

func (r *Result) fail(name, reason, message string) {
	r.Workers = append(r.Workers, WorkerResult{Name: name, Reason: reason, Message: message})
}

// Evaluate runs the Preprovisioned precheck against workers: whether each one
// already carries enough free hugepages-2Mi for a cluster sized by maxSubsys
// and vcpuCount, with minHugePagesSizeMiB as the cluster's own floor (0 when
// unset).
//
// clusterUUID is the control-plane cluster this precheck runs for
// (StorageCluster.Status.UUID), which a worker's own already-running storage
// nodes are excluded from contention by (S:5 Step 3, U-12): a pod carrying
// this cluster's clusterLabel is this check's own prior work, not something
// holding pages against it.
func Evaluate(
	ctx context.Context,
	k8sClient client.Client,
	cpClient EstimateHugePagesClient,
	workers []discovery.KubeNode,
	clusterUUID string,
	maxSubsys, vcpuCount int32,
	minHugePagesSizeMiB int64,
) (*Result, error) {
	requiredMiB, err := requiredHugePagesMiB(ctx, cpClient, maxSubsys, vcpuCount, minHugePagesSizeMiB)
	if err != nil {
		return nil, err
	}

	result := &Result{RequiredMiB: requiredMiB}
	requiredBytes := requiredMiB * mibBytes
	for _, worker := range workers {
		allocatable, ok := twoMiBAllocatableBytes(worker)
		if !ok || allocatable < requiredBytes {
			result.fail(worker.Name, ReasonInsufficientHugePages, fmt.Sprintf(
				"worker %s does not have enough preprovisioned huge pages: needs %dMi of hugepages-2Mi, reports %dMi allocatable",
				worker.Name, requiredMiB, allocatable/mibBytes))
			continue
		}

		holders, heldBytes, err := contendingPods(ctx, k8sClient, worker.Name, clusterUUID)
		if err != nil {
			return nil, err
		}
		freeBytes := allocatable - heldBytes
		if freeBytes < requiredBytes {
			result.fail(worker.Name, ReasonHugePagesContended, fmt.Sprintf(
				"worker %s's hugepages-2Mi is held by other pods (%v), leaving %dMi free of the %dMi needed",
				worker.Name, holders, freeBytes/mibBytes, requiredMiB))
			continue
		}
		result.pass(worker.Name)
	}
	return result, nil
}

// requiredHugePagesMiB is S:5 Step 1: the control plane's sizing formula,
// floored against minHugePagesSizeMiB, then rounded exactly as
// kubernetes.py's MEM_MEGA derives the real pod's hugepages-2Mi limit from
// that same floored figure. The floor is applied before the rounding because
// sbcli applies them in that order (minimum_hp_memory = max(computed,
// max_prov) precedes MEM_MEGA), and the control plane's own endpoint has no
// floor to clamp against by design: whichever side applies it also has to
// apply the rounding that follows.
func requiredHugePagesMiB(
	ctx context.Context, cpClient EstimateHugePagesClient, maxSubsys, vcpuCount int32, minHugePagesSizeMiB int64,
) (int64, error) {
	formulaMiB, err := cpClient.EstimateHugePages(ctx, maxSubsys, vcpuCount)
	if err != nil {
		return 0, fmt.Errorf("estimate huge pages: %w", err)
	}
	flooredMiB := max(formulaMiB, minHugePagesSizeMiB)
	return (flooredMiB/2)*2 + 512, nil
}

// twoMiBAllocatableBytes is S:5 Step 2: what the kubelet reports this worker
// can schedule against for hugepages-2Mi, and whether it reports the size at
// all.
func twoMiBAllocatableBytes(worker discovery.KubeNode) (int64, bool) {
	for _, hp := range worker.HugePages {
		if hp.SizeBytes == twoMiBBytes {
			return hp.AllocatableBytes, true
		}
	}
	return 0, false
}

// contendingPods is S:5 Step 3: every pod already scheduled on worker that is
// not one of clusterUUID's own storage nodes, and how much hugepages-2Mi it
// holds. Kubernetes reports a Node's hugepages-2Mi capacity and allocatable as
// static fields, not "what is free right now", so this replicates the
// scheduler's own extended-resource bookkeeping by summing requests directly
// rather than trusting either field to already reflect them.
//
// This lists every Pod rather than using a field-selector index: the
// precheck runs once per candidate worker, before any StorageNode exists, not
// on a hot path that would justify a manager-level spec.nodeName index.
func contendingPods(
	ctx context.Context, k8sClient client.Client, workerName, clusterUUID string,
) (holders []string, heldBytes int64, err error) {
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods); err != nil {
		return nil, 0, fmt.Errorf("list pods to check hugepages-2Mi contention on %s: %w", workerName, err)
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName != workerName {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue // a terminal pod holds nothing, whatever it once requested
		}
		if clusterUUID != "" && pod.Labels[clusterLabel] == clusterUUID {
			continue // this cluster's own storage node, not contention
		}
		held := podHugePagesBytes(pod)
		if held > 0 {
			holders = append(holders, pod.Name)
			heldBytes += held
		}
	}
	return holders, heldBytes, nil
}

// podHugePagesBytes sums every container's hugepages-2Mi, preferring the
// request and falling back to the limit: Kubernetes requires the two equal
// for an extended resource, and a pod that states only the limit (as
// storage_deploy_spdk.yaml.j2 itself does) still holds exactly that much.
func podHugePagesBytes(pod corev1.Pod) int64 {
	var total int64
	for _, c := range pod.Spec.Containers {
		if q, ok := c.Resources.Requests[hugePages2MiResource]; ok {
			total += q.Value()
			continue
		}
		if q, ok := c.Resources.Limits[hugePages2MiResource]; ok {
			total += q.Value()
		}
	}
	return total
}

// MinHugePagesSizeMiB parses a ClusterTemplate/StorageCluster MinHugePagesSize
// string ("100G" or "1T") into MiB, 0 when raw is unset or not parsable: an
// unset floor applies no clamp in requiredHugePagesMiB. Parsed exactly as
// creationParams (internal/controllers/cluster/storagecluster_controller.go)
// already parses the same field for the control plane's own create call, so
// this precheck floors against the same bytes the cluster will.
func MinHugePagesSizeMiB(raw string) int64 {
	bytes := utils.ParseSize(raw, "si/iec", "", false)
	if bytes == nil {
		return 0
	}
	return *bytes / mibBytes
}
