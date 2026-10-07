// The result a validation Job reports, and how the operator reads it back.
//
// A Job that completed only says its process exited zero, and the validate
// mode exits zero both when it validated the target's paths and when it found
// the host holding no connection to the subsystem and skipped it. The result is
// what tells the two apart. The Job writes it as its container's termination
// message, which the kubelet copies into the pod's status, so the operator
// reads it without a log stream and without a shared volume.
//
// It lives here, beside the Job builder and the connection wire format,
// because the Job's binary and the operator are the two ends of one contract.

package volumemigration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ValidationResultPath is where the validation Job writes its result, and the
// container's terminationMessagePath. It is not the kubelet's default
// /dev/termination-log, because the Job mounts the host's /dev over the
// container's.
const ValidationResultPath = "/tmp/validation-result"

// ValidationResultPathEnv carries ValidationResultPath into the Job, so the
// binary writes where the container spec says the kubelet reads.
const ValidationResultPathEnv = "VMIG_RESULT_PATH"

// ValidationOutcome is what one host's validation concluded.
type ValidationOutcome string

const (
	// ValidationValidated is a host whose target paths are connected, live,
	// and in the expected pre-cutover state. It is the only passing outcome.
	ValidationValidated ValidationOutcome = "validated"
	// ValidationSkipped is a host holding no connection to the subsystem, so
	// nothing was checked on it.
	ValidationSkipped ValidationOutcome = "skipped"
)

// ValidationResult is the termination message of a validation Job.
type ValidationResult struct {
	Outcome ValidationOutcome `json:"outcome"`
	Detail  string            `json:"detail,omitempty"`
}

// Passed reports whether the host is ready for the cutover.
func (r ValidationResult) Passed() bool { return r.Outcome == ValidationValidated }

// WriteValidationResult writes the result where the kubelet reads the
// termination message. An empty path writes nothing, which is a Job started by
// an operator that does not read results.
func WriteValidationResult(path string, result ValidationResult) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode the validation result: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write the validation result to %s: %w", path, err)
	}
	return nil
}

// ReadValidationResult reads the result a finished validation Job reported
// through its pod's termination message.
//
// A Job with no pod, a container that has not terminated, and a message that
// is empty or does not parse are each an error: a Job that reports nothing
// validated nothing, and reading it as a pass is the defect this exists for.
func ReadValidationResult(
	ctx context.Context, reader client.Reader, job *batchv1.Job, container string,
) (ValidationResult, error) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name}); err != nil {
		return ValidationResult{}, fmt.Errorf("list the pods of Job %s: %w", job.Name, err)
	}

	for i := range pods.Items {
		for _, status := range pods.Items[i].Status.ContainerStatuses {
			terminated := status.State.Terminated
			if status.Name != container || terminated == nil || terminated.ExitCode != 0 {
				continue
			}
			message := strings.TrimSpace(terminated.Message)
			if message == "" {
				return ValidationResult{}, fmt.Errorf("the Job %s reported no validation result", job.Name)
			}
			var result ValidationResult
			if err := json.Unmarshal([]byte(message), &result); err != nil {
				return ValidationResult{}, fmt.Errorf(
					"the Job %s reported a validation result that does not parse (%q): %w", job.Name, message, err)
			}
			return result, nil
		}
	}
	return ValidationResult{}, fmt.Errorf("the Job %s has no pod whose %s container exited successfully",
		job.Name, container)
}
