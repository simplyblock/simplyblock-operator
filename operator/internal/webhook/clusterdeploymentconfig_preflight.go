// The preflight gate on approval: a draft written by a discovery run with an
// unwaived Fail finding is refused until the finding is fixed or waived.

package webhook

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// PreflightFailed is the reason approval is refused under.
const PreflightFailed = "PreflightFailed"

// preflightProblems reads the run that wrote the draft and returns its unwaived
// Fail findings as one problem. The waivers are read from the run's spec rather
// than its status, so a waiver counts before the controller records it. A draft
// with no run, or whose run is gone, has nothing to gate on.
func (v *ClusterDeploymentConfigValidator) preflightProblems(
	ctx context.Context, namespace string, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]problem, error) {
	run := config.Labels[nodeprobe.LabelRun]
	if run == "" {
		return nil, nil
	}
	var ops simplyblockv1alpha2.OperatorOps
	if err := v.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: run}, &ops); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	var ignore []string
	if ops.Spec.Discover != nil {
		ignore = ops.Spec.Discover.IgnoreFindings
	}
	var lines []string
	for _, f := range ops.Status.Findings {
		if f.Severity != simplyblockv1alpha2.PreflightSeverityFail || slices.Contains(ignore, f.Reason) {
			continue
		}
		line := fmt.Sprintf("%s: %s", f.Reason, f.Message)
		if f.Remedy != "" {
			line += " (" + f.Remedy + ")"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil, nil
	}
	return []problem{{
		reason: PreflightFailed,
		message: fmt.Sprintf(
			"discovery run %s found %d Fail finding(s): %s. Fix them and run discovery again, or name "+
				"their reason in the run's spec.discover.ignoreFindings",
			run, len(lines), strings.Join(lines, "; ")),
	}}, nil
}
