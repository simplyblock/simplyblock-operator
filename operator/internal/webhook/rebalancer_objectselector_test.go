// The rebalancer injector's object selector, evaluated against an SPDK pod and
// an unrelated one. The selector lives in a Kustomize patch that webhook markers
// cannot express, so nothing else ties it to the label the SPDK pod carries.

package webhook

import (
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// The injector registered for every pod creation in every namespace, so the API
// server sent each one to the operator only for Handle to drop all but the SPDK
// pods. The object selector keeps the API server from calling it for the rest.
func TestRebalancerInjectorSelectsOnlySpdkPods(t *testing.T) {
	raw, err := os.ReadFile("../../config/webhook/objectselector_patch.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var patch struct {
		Webhooks []struct {
			Name           string                `json:"name"`
			ObjectSelector *metav1.LabelSelector `json:"objectSelector"`
		} `json:"webhooks"`
	}
	if err := yaml.Unmarshal(raw, &patch); err != nil {
		t.Fatal(err)
	}

	var selector *metav1.LabelSelector
	for _, w := range patch.Webhooks {
		if w.Name == "simplyblock-rebalancer-injector.simplyblock.io" {
			selector = w.ObjectSelector
		}
	}
	if selector == nil {
		t.Fatal("the rebalancer injector has no object selector")
	}
	matcher, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		t.Fatal(err)
	}

	spdkPod := labels.Set{"role": utils.LabelSpdkProxyRole, appLabel: spdkAppPrefix + "4420"}
	if !matcher.Matches(spdkPod) {
		t.Errorf("an SPDK pod labeled %v is not sent to the injector", spdkPod)
	}
	otherPod := labels.Set{appLabel: "nginx"}
	if matcher.Matches(otherPod) {
		t.Errorf("a pod labeled %v is sent to the injector", otherPod)
	}
}
