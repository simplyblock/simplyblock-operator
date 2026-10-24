// Building a discovery run, an OperatorOps with action Discover, without a
// bundle: the run a support agent asks a customer to start, with the settings
// a previous run or a symptom calls for.
//
// It is apart from the CDC editor because it needs no fleet: every setting of a
// run is checked against the API, not against a bundle's reports. A bundle,
// when there is one, only seeds the settings with those of the run it carries.

package opsedit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/webhook"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/admission"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/manifest"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

// Request is a discovery run as the editor states it: its name, its namespace,
// and spec.discover in the API's own shape.
type Request struct {
	Name      string                           `json:"name"`
	Namespace string                           `json:"namespace"`
	Discover  simplyblockv1alpha2.DiscoverSpec `json:"discover"`
}

// Finding is one reason the run would be refused, and which check refuses it:
// "admission" for the CRD's schema and CEL rules, "webhook" for the operator's
// admission webhook.
type Finding struct {
	Source  string `json:"source"`
	Message string `json:"message"`
}

// Prefill is a run with the defaults, or, given a bundle, a run with the
// settings of the bundle's run under a new name: a finished run never runs
// again, so a new run is a new name.
func Prefill(b *supportbundle.Bundle) Request {
	req := Request{Name: "discovery", Namespace: "simplyblock"}
	if b == nil || b.Run == nil {
		return req
	}
	req.Name, req.Namespace = nextName(b.Run.Name), b.Run.Namespace
	if b.Run.Spec.Discover != nil {
		req.Discover = *b.Run.Spec.Discover.DeepCopy()
		// A run whose draft name is taken adopts that draft and leaves it as it
		// is, so the new run's draft is named after the new run.
		req.Discover.ConfigName = ""
	}
	return req
}

// nextName is a run's name with its number counted up, keeping its width:
// discovery becomes discovery-2, discovery-2 becomes discovery-3, and
// discover-fleet-02 becomes discover-fleet-03.
func nextName(name string) string {
	i := strings.LastIndex(name, "-")
	if i <= 0 || i == len(name)-1 {
		return name + "-2"
	}
	digits := name[i+1:]
	n, err := strconv.Atoi(digits)
	if err != nil || strings.ContainsAny(digits, "+- ") {
		return name + "-2"
	}
	return fmt.Sprintf("%s-%0*d", name[:i], len(digits), n+1)
}

// Build is the OperatorOps a request describes. It fails for a request no run
// can be made of.
func Build(req Request) (*simplyblockv1alpha2.OperatorOps, error) {
	var problems []string
	for _, msg := range validation.IsDNS1123Subdomain(req.Name) {
		problems = append(problems, fmt.Sprintf("the name %q: %s", req.Name, msg))
	}
	for _, msg := range validation.IsDNS1123Label(req.Namespace) {
		problems = append(problems, fmt.Sprintf("the namespace %q: %s", req.Namespace, msg))
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	ops := &simplyblockv1alpha2.OperatorOps{
		TypeMeta:   metav1.TypeMeta{APIVersion: simplyblockv1alpha2.GroupVersion.String(), Kind: "OperatorOps"},
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: req.Namespace},
		Spec:       simplyblockv1alpha2.OperatorOpsSpec{Action: simplyblockv1alpha2.OperatorOpsActionDiscover},
	}
	if discover := req.Discover.DeepCopy(); !isEmpty(discover) {
		ops.Spec.Discover = discover
	}
	return ops, nil
}

// isEmpty reports whether spec.discover states nothing, which leaves it out.
func isEmpty(d *simplyblockv1alpha2.DiscoverSpec) bool {
	raw, err := json.Marshal(d)
	return err == nil && string(raw) == "{}"
}

// Check is every reason the API server would refuse the run: the CRD's schema
// and CEL rules, and the operator's admission webhook, called as the API server
// calls it for a create.
func Check(ops *simplyblockv1alpha2.OperatorOps) []Finding {
	var out []Finding
	refused, err := admission.Validate(ops)
	if err != nil {
		out = append(out, Finding{Source: "admission", Message: "admission's rules could not be checked: " + err.Error()})
	}
	for _, message := range refused {
		out = append(out, Finding{Source: "admission", Message: message})
	}

	raw, err := json.Marshal(ops)
	if err != nil {
		return append(out, Finding{Source: "webhook", Message: err.Error()})
	}
	response := (&webhook.OperatorOpsValidator{}).Handle(context.Background(), ctrladmission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw}},
	})
	if !response.Allowed {
		message := "refused"
		if response.Result != nil {
			message = response.Result.Message
		}
		out = append(out, Finding{Source: "webhook", Message: message})
	}
	return out
}

// Render is the run as YAML to apply.
func Render(ops *simplyblockv1alpha2.OperatorOps) ([]byte, error) {
	return manifest.Render(ops, simplyblockv1alpha2.GroupVersion.String(), "OperatorOps")
}
