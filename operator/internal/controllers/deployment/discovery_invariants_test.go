// What must hold of every document the discovery run writes, whatever fleet it
// was written from.
//
// The recorded expectation beside each case says what the run produced. It
// cannot say whether that was right: it was written by running the code that
// produced it, so it asserts what the code already did. This file is the other
// half, and every check in it is made against something written for another
// purpose by somebody else:
//
//   - A real apiserver with this repository's own CRDs installed. A draft it
//     refuses is a document nobody can apply, however plausible the YAML looks,
//     and the schema and its CEL rules are generated from the API types rather
//     than restated here.
//   - The ClusterDeploymentConfig controller's own draft validation, which is
//     what decides whether a reviewer can approve the thing. It was written for
//     hand-written documents and knows nothing about discovery.
//   - The probe reports themselves. Every worker and every device a draft names
//     has to trace back to a report, which is the one property a generator can
//     violate silently and catastrophically.
//
// A finding here is a finding about the generator rather than about a fixture,
// which is the whole reason for keeping the two files apart.

package deployment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// TestEveryDraftedDocumentIsOneTheAPIServerAccepts applies what every case
// produced to a real apiserver.
//
// This is the check the recorded expectations cannot make. A golden file says
// the run wrote this YAML; only the apiserver says whether the YAML is a
// document that can exist, and the schema it decides with is generated from the
// API types rather than written for this test.
func TestEveryDraftedDocumentIsOneTheAPIServerAccepts(t *testing.T) {
	api := apiServer(t)
	ctx := context.Background()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "discovery-cases"}}
	if err := api.Create(ctx, namespace); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create the namespace to apply into: %v", err)
	}

	var refused []string
	for _, c := range withoutRecordedGaps(t, loadDiscoveryCases(t)) {
		out := runDiscoveryCase(t, c)
		if out.Config == nil {
			continue
		}

		// The name is the case's rather than the run's, so that a hundred and
		// fifty documents can be applied into one namespace without colliding,
		// and the length a run would have produced is checked separately below.
		applied := out.Config.DeepCopy()
		applied.Namespace = namespace.Name
		applied.Name = applyableName(c.Name)
		applied.ResourceVersion = ""

		if err := api.Create(ctx, applied); err != nil {
			refused = append(refused, fmt.Sprintf("%s: %s", c.Name, err.Error()))
			continue
		}
		_ = api.Delete(ctx, applied)
	}

	sort.Strings(refused)
	for _, problem := range refused {
		t.Errorf("the run wrote a document the apiserver refuses: %s", problem)
	}
}

// TestEveryDraftedClusterNameIsOneAClusterCanHave checks the half of the name
// the apiserver cannot: the cluster the document proposes does not exist yet, so
// nothing refuses a name too long for it until the run tries to create it.
func TestEveryDraftedClusterNameIsOneAClusterCanHave(t *testing.T) {
	const clusterNameLimit = 63

	var findings []string
	for _, c := range withoutRecordedGaps(t, loadDiscoveryCases(t)) {
		out := runDiscoveryCase(t, c)
		if out.Config == nil || out.Config.Spec.Cluster == nil {
			continue
		}
		if name := out.Config.Spec.Cluster.Name; len(name) > clusterNameLimit {
			findings = append(findings, fmt.Sprintf("%s: %q is %d characters",
				c.Name, name, len(name)))
		}
	}

	sort.Strings(findings)
	for _, problem := range findings {
		t.Errorf("the run proposed a cluster whose name no StorageCluster can carry: %s", problem)
	}
}

// knownUnbuildable are the two findings a generated document is currently
// allowed to carry, each recorded as a gap in discovery-generator-rules.md.
//
// They are listed rather than skipped so that the list is the specification: a
// document failing its own validation for any other reason is a generator that
// has started producing something new, and that is what this test is for.
var knownUnbuildable = map[string]string{
	// A worker presenting no interface that would serve is drafted anyway, with
	// the group's management interface empty. The rules document records it as
	// the generator writing a group it already knows cannot deploy.
	NoManagementInterface: "the ladder found no interface and the group was drafted regardless",

	// The grouper splits workers on their management interface while the
	// expansion binds one interface per cluster, so a fleet whose machines name
	// their NICs differently produces exactly the document this refuses.
	WorkerNotFound: "the groups name different management interfaces",
}

// TestEveryDraftedDocumentPassesItsOwnValidation runs each document through the
// check a reviewer's document gets before it can be approved.
//
// A draft that fails it is one the reviewer is told is wrong, which for a
// hand-written document is the point and for a generated one is the generator
// handing somebody a document it already knows cannot deploy.
func TestEveryDraftedDocumentPassesItsOwnValidation(t *testing.T) {
	var findings []string
	for _, c := range loadDiscoveryCases(t) {
		out := runDiscoveryCase(t, c)
		if out.Config == nil {
			continue
		}
		for _, found := range validateDraft(t, c, out.Config) {
			if _, known := knownUnbuildable[found.reason]; known {
				continue
			}
			findings = append(findings, fmt.Sprintf("%s: %s: %s", c.Name, found.reason, found.message))
		}
	}

	sort.Strings(findings)
	for _, problem := range findings {
		t.Errorf("the run wrote a document its own validation rejects: %s", problem)
	}
}

// TestEveryDraftedDocumentNamesOnlyWhatWasReported checks the draft against the
// reports it was built from.
//
// It is the property that cannot be read off any single document: a device
// named for a worker that never reported it is a cluster told to take a disk
// that may belong to something else, and nothing downstream would catch it,
// because the address is well formed and the worker exists.
func TestEveryDraftedDocumentNamesOnlyWhatWasReported(t *testing.T) {
	var findings []string
	for _, c := range loadDiscoveryCases(t) {
		out := runDiscoveryCase(t, c)
		if out.Config == nil {
			continue
		}
		for _, problem := range unreportedContent(c, out.Config) {
			findings = append(findings, fmt.Sprintf("%s: %s", c.Name, problem))
		}
	}

	sort.Strings(findings)
	for _, problem := range findings {
		t.Errorf("the run named something no probe reported: %s", problem)
	}
}

// TestNoDraftedDocumentPlacesAWorkerTwice is the invariant the expansion
// depends on.
//
// A StorageNode is identified by its cluster, its worker, and its slot, so a
// worker in two groups expands into one node carrying whichever group the
// expansion reached first. The document's own validation reports it; a
// generator should be incapable of producing it.
func TestNoDraftedDocumentPlacesAWorkerTwice(t *testing.T) {
	var findings []string
	for _, c := range loadDiscoveryCases(t) {
		out := runDiscoveryCase(t, c)
		if out.Config == nil {
			continue
		}
		if repeated := duplicateWorkers(out.Config); len(repeated) > 0 {
			findings = append(findings, fmt.Sprintf("%s: %s", c.Name, strings.Join(repeated, ", ")))
		}
	}

	sort.Strings(findings)
	for _, problem := range findings {
		t.Errorf("the run placed a worker in more than one group: %s", problem)
	}
}

// withoutRecordedGaps drops the cases that exist to hold today's output against
// a finding.
//
// A case carrying a gap number is one the document already says is wrong, and
// the expectation beside it is the record that it has not silently changed.
// Holding those to a property they are known to break would turn every finding
// into a failing test and leave no signal for the cases that should hold.
func withoutRecordedGaps(t *testing.T, cases []discoveryCase) []discoveryCase {
	t.Helper()

	var out []discoveryCase
	for _, c := range cases {
		body, err := os.ReadFile(filepath.Join(c.Dir, caseFile))
		if err != nil {
			t.Fatalf("read %s: %v", c.Dir, err)
		}
		if strings.Contains(string(body), "**Gap.**") {
			continue
		}
		out = append(out, c)
	}
	return out
}

// validateDraft runs the document through the ClusterDeploymentConfig
// controller's own draft validation, against the same nodes the case carries.
func validateDraft(
	t *testing.T, c discoveryCase, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) []finding {
	t.Helper()

	scheme := opsScheme(t)
	objects := append([]client.Object{config.DeepCopy()}, c.Objects...)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	reconciler := &ClusterDeploymentConfigReconciler{Client: fakeClient, Scheme: scheme}
	findings, err := reconciler.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("%s: validate the document: %v", c.Name, err)
	}
	return findings
}

// unreportedContent is every worker and every device address the document names
// that no probe report accounts for.
func unreportedContent(
	c discoveryCase, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) []string {
	reported := map[string]map[string]struct{}{}
	for _, object := range c.Objects {
		cm, isConfigMap := object.(*corev1.ConfigMap)
		if !isConfigMap {
			continue
		}
		report, err := nodeprobe.ReportFromConfigMap(cm)
		if err != nil {
			continue
		}

		names := map[string]struct{}{}
		for _, device := range report.Devices {
			if device.PCIAddress != "" {
				names[device.PCIAddress] = struct{}{}
			}
			if device.Path != "" {
				names[device.Path] = struct{}{}
			}
		}
		for _, controller := range report.NVMeControllers {
			names[controller.Address] = struct{}{}
		}
		reported[report.Node] = names
	}

	var problems []string
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			var addresses []string
			if group.Devices != nil {
				addresses = append(addresses, group.Devices.NVMe...)
				addresses = append(addresses, group.Devices.Block...)
			}
			for _, worker := range group.Workers {
				names, known := reported[worker]
				if !known {
					problems = append(problems, "worker "+worker+" has no report")
					continue
				}
				for _, address := range addresses {
					if _, found := names[address]; !found {
						problems = append(problems, fmt.Sprintf(
							"%s is named for worker %s, which reported no such device",
							address, worker))
					}
				}
			}
		}
	}
	slices.Sort(problems)
	return slices.Compact(problems)
}

// applyableName is a name for the copy of a document applied to the apiserver,
// derived from the case's path so that a refusal names the case.
func applyableName(caseName string) string {
	name := strings.ReplaceAll(caseName, "/", "-")
	if len(name) > 200 {
		name = name[:200]
	}
	return strings.Trim(name, "-")
}
