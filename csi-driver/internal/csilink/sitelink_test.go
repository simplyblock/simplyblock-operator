package csilink

// The CSI controller's end of the site link trusts the CA the site's fleet
// agent publishes, read through the API because it lives in the agent's
// namespace.

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func caConfigMap(data string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: siteLinkCAConfigMap, Namespace: "agent-ns"},
		Data:       map[string]string{"ca.crt": data},
	}
}

// A published CA that is not a certificate is an error, not a plaintext dial.
func TestASiteLinkCAThatIsNotACertificateIsRefused(t *testing.T) {
	_, err := siteLinkDialer(context.Background(), fake.NewClientset(caConfigMap("not a pem")), "agent-ns", "agent:9443")
	if err == nil {
		t.Error("a CA that is not PEM produced a dialer")
	}
}

// A CA the agent has not published yet is waited for until the context ends.
func TestASiteLinkWaitsForTheAgentsCA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := siteLinkDialer(ctx, fake.NewClientset(), "agent-ns", "agent:9443"); err == nil {
		t.Error("a dialer was built before the agent published its CA")
	}
}
