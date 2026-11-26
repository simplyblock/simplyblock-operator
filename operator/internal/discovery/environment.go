// How a draft reads which provider built each worker, for a cluster whose
// workers come from more than one: Google Anthos joins EKS and OKE node groups
// to a GKE control plane. The run concludes the cluster's environment from the
// control plane, and a worker whose own markers name another provider is
// drafted into a group that states it.

package discovery

import (
	"fmt"
	"slices"
	"strings"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// wholeCluster reports whether an environment is what every worker of its
// cluster runs, so that no worker is ever on another one.
func wholeCluster(environment simplyblockv1alpha2.KubernetesEnvironment) bool {
	return environment == simplyblockv1alpha2.KubernetesEnvironmentOpenShift ||
		environment == simplyblockv1alpha2.KubernetesEnvironmentTalos
}

// assignEnvironments sets the environment of every worker whose own markers name
// another distribution than the cluster's, and reports them.
//
// A worker with no marker of its own is left on the cluster's environment,
// because it says nothing about itself. OpenShift and Talos are neither
// assigned nor assigned to, because each runs a whole cluster and admission
// refuses a group environment on either side of one.
func (p *Plan) assignEnvironments(cluster simplyblockv1alpha2.KubernetesEnvironment) {
	if wholeCluster(cluster) {
		return
	}
	byEnvironment := map[simplyblockv1alpha2.KubernetesEnvironment][]string{}
	for i := range p.Workers {
		worker := &p.Workers[i]
		own := simplyblockv1alpha2.KubernetesEnvironment(worker.Kube.Distribution)
		if own == "" || own == cluster || wholeCluster(own) {
			continue
		}
		worker.Environment = own
		byEnvironment[own] = append(byEnvironment[own], worker.Name)
	}
	if len(byEnvironment) == 0 {
		return
	}

	environments := make([]simplyblockv1alpha2.KubernetesEnvironment, 0, len(byEnvironment))
	for environment := range byEnvironment {
		environments = append(environments, environment)
	}
	slices.Sort(environments)
	clauses := make([]string, 0, len(environments))
	for _, environment := range environments {
		clauses = append(clauses, fmt.Sprintf("%s on %s",
			strings.Join(byEnvironment[environment], ", "), environment))
	}
	p.Findings = append(p.Findings, Finding{Reason: WorkerEnvironmentDiffers, Message: fmt.Sprintf(
		"the cluster's control plane is %s and some workers run on another provider's Kubernetes "+
			"(%s), so their groups state their own environment",
		orUnknownEnvironment(cluster), strings.Join(clauses, "; "))})
}

// orUnknownEnvironment names an environment, or says that none was concluded.
func orUnknownEnvironment(environment simplyblockv1alpha2.KubernetesEnvironment) string {
	if environment == "" {
		return "of an unknown distribution"
	}
	return string(environment)
}
