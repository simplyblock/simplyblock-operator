// Tests for the SimplyblockDriver singleton, which is the rule that a
// Kubernetes cluster holds one CSI driver deployment.
//
// The cases are design-simplyblockdriver.md §3.4 and its test plan's I-18 to
// I-22: the first object is admitted, a second is denied wherever it is written,
// and an edit to the one that exists is not mistaken for a second.

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func newDriverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := simplyblockv1alpha2.AddToScheme(s); err != nil {
		t.Fatalf("add simplyblock v1alpha2 scheme: %v", err)
	}
	return s
}

func testDriverObject(name, namespace string) *simplyblockv1alpha2.SimplyblockDriver {
	return &simplyblockv1alpha2.SimplyblockDriver{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: simplyblockv1alpha2.SimplyblockDriverSpec{
			Image: "quay.io/simplyblock-io/spdkcsi:v26.2.6",
		},
	}
}

func driverRaw(t *testing.T, d *simplyblockv1alpha2.SimplyblockDriver) runtime.RawExtension {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal SimplyblockDriver: %v", err)
	}
	return runtime.RawExtension{Raw: b}
}

func TestSimplyblockDriverValidator(t *testing.T) {
	tests := []struct {
		name     string
		existing []client.Object
		op       admissionv1.Operation
		incoming *simplyblockv1alpha2.SimplyblockDriver
		allowed  bool
		// namesHolder is the object the denial must name, so that the message
		// says where the deployment already lives rather than only that it does.
		namesHolder string
	}{
		{
			// I-20
			name:     "the first driver in an empty cluster is admitted",
			existing: nil,
			op:       admissionv1.Create,
			incoming: testDriverObject("simplyblock", "simplyblock"),
			allowed:  true,
		},
		{
			// I-18
			name:        "a second driver in the same namespace is denied",
			existing:    []client.Object{testDriverObject("simplyblock", "simplyblock")},
			op:          admissionv1.Create,
			incoming:    testDriverObject("second", "simplyblock"),
			allowed:     false,
			namesHolder: "simplyblock/simplyblock",
		},
		{
			// I-19
			name:        "a second driver in another namespace is denied",
			existing:    []client.Object{testDriverObject("simplyblock", "simplyblock")},
			op:          admissionv1.Create,
			incoming:    testDriverObject("simplyblock", "tenant-b"),
			allowed:     false,
			namesHolder: "simplyblock/simplyblock",
		},
		{
			// I-21: the rule is CREATE, so editing the object that exists is not
			// a second one. A rule over every operation would lock the running
			// deployment's own spec.
			name:     "updating the only driver is admitted",
			existing: []client.Object{testDriverObject("simplyblock", "simplyblock")},
			op:       admissionv1.Update,
			incoming: testDriverObject("simplyblock", "simplyblock"),
			allowed:  true,
		},
		{
			// I-22: the object was deleted, so the cluster holds none again.
			name:     "creating a driver after the only one was deleted is admitted",
			existing: nil,
			op:       admissionv1.Create,
			incoming: testDriverObject("replacement", "simplyblock"),
			allowed:  true,
		},
		{
			name:     "a delete is not intercepted",
			existing: []client.Object{testDriverObject("simplyblock", "simplyblock")},
			op:       admissionv1.Delete,
			incoming: testDriverObject("simplyblock", "simplyblock"),
			allowed:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().
				WithScheme(newDriverScheme(t)).
				WithObjects(tc.existing...).
				Build()
			v := &SimplyblockDriverValidator{Client: c}

			resp := v.Handle(context.Background(), admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Operation: tc.op,
					Object:    driverRaw(t, tc.incoming),
				},
			})

			if resp.Allowed != tc.allowed {
				msg := ""
				if resp.Result != nil {
					msg = resp.Result.Message
				}
				t.Fatalf("Allowed = %v, want %v (msg: %s)", resp.Allowed, tc.allowed, msg)
			}
			if tc.namesHolder == "" {
				return
			}
			if resp.Result == nil || !strings.Contains(resp.Result.Message, tc.namesHolder) {
				t.Fatalf("denial does not name %q: %+v", tc.namesHolder, resp.Result)
			}
		})
	}
}
