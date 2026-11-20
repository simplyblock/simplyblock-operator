// How a draft reads each worker's failure domain off its Node object
// (design-superclusters.md §5.7). A zone is a failure domain, and a fleet whose
// installer labels racks names that label in the run instead. Missing or
// unusable topology is reported rather than guessed, because the draft is a
// proposal a reviewer corrects, and labeling the workers and running discovery
// again is the correction.

package discovery

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// failureDomainPattern is what NodeGroup.failureDomain accepts, mirrored from
// the API's own markers.
var failureDomainPattern = regexp.MustCompile(`^[a-zA-Z0-9]([-_.a-zA-Z0-9]*[a-zA-Z0-9])?$`)

// maxFailureDomainLength is the field's MaxLength.
const maxFailureDomainLength = 63

// failureDomainLabelOf is the label the run reads failure domains from, the
// zone label unless the run names another.
func failureDomainLabelOf(run *simplyblockv1alpha2.DiscoverSpec) string {
	if run != nil && run.FailureDomainLabel != "" {
		return run.FailureDomainLabel
	}
	return corev1.LabelTopologyZone
}

// assignFailureDomains sets every worker's failure domain from the label, and
// reports the workers whose value the field refuses and, in a fleet where some
// workers have one, the workers that have none.
func (p *Plan) assignFailureDomains(label string) {
	var unlabeled []string
	invalid := 0
	for i := range p.Workers {
		worker := &p.Workers[i]
		value := worker.Kube.Labels[label]
		switch {
		case value == "":
			unlabeled = append(unlabeled, worker.Name)
		case len(value) > maxFailureDomainLength || !failureDomainPattern.MatchString(value):
			invalid++
			p.Findings = append(p.Findings, Finding{Reason: FailureDomainInvalid, Message: fmt.Sprintf(
				"worker %s carries %s=%q, which is not a failure domain (at most %d letters, digits, "+
					"'-', '_', or '.', starting and ending with a letter or digit), so it has none in the draft",
				worker.Name, label, value, maxFailureDomainLength)})
		default:
			worker.FailureDomain = value
		}
	}

	labeled := len(p.Workers) - len(unlabeled) - invalid
	if labeled > 0 && len(unlabeled) > 0 {
		p.Findings = append(p.Findings, Finding{Reason: TopologyIncomplete, Message: fmt.Sprintf(
			"%s %s no %s label while other workers do, so the draft gives %s no failure domain and "+
				"does not enable failure domains: label %s and run discovery again",
			strings.Join(unlabeled, ", "), plural(len(unlabeled), "carries", "carry"), label,
			plural(len(unlabeled), "it", "them"), plural(len(unlabeled), "it", "them"))})
	}
	p.FailureDomains = len(p.Workers) > 0 && labeled == len(p.Workers)
}
