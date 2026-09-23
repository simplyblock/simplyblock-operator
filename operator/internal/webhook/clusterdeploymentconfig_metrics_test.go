// The counter an admission rejection leaves behind.
//
// A rejected approval fails the request, so there is no object to raise an event
// on and nothing in the cluster records that it happened. The metric is the only
// signal that path has, and it is the pair to read the controller's validation
// counter against: a rejection whose reason draft validation never reported
// first is a gap in the validation, because the reviewer should have been told
// before they wrote the approval.

package webhook

import (
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/simplyblock/simplyblock-operator/internal/controllers/deployment"
)

// approvalRejectionsMetric is the series this file reads, named rather than
// reached for: the counter is the deployment package's, and a scraper is the
// only consumer either package has in common.
const approvalRejectionsMetric = "simplyblock_clusterdeploymentconfig_approval_rejections_total"

func TestARejectedApprovalIsCountedByReason(t *testing.T) {
	cases := []struct {
		name     string
		reason   string
		rejected func(t *testing.T) int
	}{{
		name:   "a worker that is not a node",
		reason: deployment.WorkerNotFound,
		rejected: func(t *testing.T) int {
			config := testConfig()
			config.Spec.NodeSets[0].Groups[0].Workers = []string{"worker-9"}
			return rejectionsAfter(t, deployment.WorkerNotFound, func() {
				mustDeny(t, approve(t, []client.Object{testWorker("worker-1")}, config))
			})
		},
	}, {
		name:   "a clusterRef resolving to nothing",
		reason: deployment.ClusterNotFound,
		rejected: func(t *testing.T) int {
			return rejectionsAfter(t, deployment.ClusterNotFound, func() {
				mustDeny(t, approve(t, []client.Object{testWorker("worker-1")},
					grows(testConfig(), testDeploymentCluster)))
			})
		},
	}, {
		name:   "a spec edited after approval",
		reason: deployment.SpecImmutable,
		rejected: func(t *testing.T) int {
			old := approved(testConfig())
			edited := approved(testConfig())
			edited.Spec.NodeSets[0].Groups[0].Workers = []string{"worker-2"}
			return rejectionsAfter(t, deployment.SpecImmutable, func() {
				mustDeny(t, review(t, []client.Object{testWorker("worker-1")},
					admissionv1.Update, old, edited))
			})
		},
	}, {
		name:   "an approval withdrawn",
		reason: deployment.ApprovalWithdrawn,
		rejected: func(t *testing.T) int {
			old := approved(testConfig())
			withdrawn := testConfig()
			withdrawn.Spec.Approved = false
			return rejectionsAfter(t, deployment.ApprovalWithdrawn, func() {
				mustDeny(t, review(t, []client.Object{testWorker("worker-1")},
					admissionv1.Update, old, withdrawn))
			})
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if counted := tc.rejected(t); counted != 1 {
				t.Errorf("the rejection raised %d under %s, want 1", counted, tc.reason)
			}
		})
	}
}

// An admitted approval counts nothing. A counter that also rose on the happy
// path would make the rate meaningless, which is the only thing it is read as.
func TestAnAdmittedApprovalIsCountedNowhere(t *testing.T) {
	counted := rejectionsAfter(t, deployment.WorkerNotFound, func() {
		mustAllow(t, approve(t, []client.Object{testWorker("worker-1")}, testConfig()))
	})
	if counted != 0 {
		t.Errorf("an admitted approval raised %d rejections", counted)
	}
}

// rejectionsAfter is how many rejections of one reason the given admission
// raised, measured as a difference so that the test says nothing about what ran
// before it.
func rejectionsAfter(t *testing.T, reason string, admit func()) int {
	t.Helper()
	before := approvalRejections(t, reason)
	admit()
	return approvalRejections(t, reason) - before
}

// approvalRejections reads the counter out of the registry a scraper would read
// it from, which is the only handle a test in this package has on a metric
// declared in another.
func approvalRejections(t *testing.T, reason string) int {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather the metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != approvalRejectionsMetric {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if labels["namespace"] == testDeploymentNamespace && labels["reason"] == reason {
				return int(metric.GetCounter().GetValue())
			}
		}
	}
	return 0
}
