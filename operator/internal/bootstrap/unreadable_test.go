// Telling a configuration that is absent from one that could not be read.
//
// They look the same to a caller that only sees an empty Config, and they mean
// opposite things. Absent is an installation that stated nothing, and the
// operator's own behavior is the right answer for it. Unread is an installation
// that may have stated anything at all, including that it wants no run, and
// proceeding on the operator's defaults is how a managed installation ends up
// with the run its `enabled: false` was written to suppress.
//
// So the distinction is a sentinel rather than a log line, because a caller has
// to be able to branch on it.

package bootstrap

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// refusingReader answers every read the way an API server does when the caller
// may not look, or when it cannot be reached at all.
func refusingReader(t *testing.T, err error) client.Client {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return err
			},
		}).
		Build()
}

// A read that failed is reported as one, and the sentinel is what says so. The
// message alone is not enough: a caller deciding whether to act on an empty
// configuration cannot parse prose.
func TestAConfigurationThatCouldNotBeReadSaysSo(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Resource: "configmaps"}, ConfigMapName, errors.New("no permission"))

	_, err := Load(context.Background(), refusingReader(t, forbidden), theNamespace)
	if err == nil {
		t.Fatal("a configuration that could not be read was reported as read")
	}
	if !errors.Is(err, ErrUnreadable) {
		t.Errorf("the failure is %v, and a caller has to be able to tell it from an "+
			"absent or unparsable document", err)
	}
}

// A document that is present and malformed is not the same failure. It was
// read, so what it says is known: nothing usable. That is the case the
// documented fallback was written for, and a caller may act on it.
func TestAMalformedDocumentIsNotAnUnreadableOne(t *testing.T) {
	_, err := loadFrom(t, configMap("enabled: true\n  name: [unbalanced\n"))
	if err == nil {
		t.Fatal("a malformed document was accepted")
	}
	if errors.Is(err, ErrUnreadable) {
		t.Errorf("a malformed document reports as unreadable (%v), which would hold "+
			"an installation that has a real answer: the document is there and it is junk", err)
	}
}

// An absent ConfigMap is neither. It is the ordinary state of an installation
// that predates the chart writing one, and it is not a failure at all.
func TestAnAbsentConfigMapIsNotAFailure(t *testing.T) {
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	config, err := Load(context.Background(), c, theNamespace)
	if err != nil {
		t.Fatalf("an absent ConfigMap reported %v", err)
	}
	if !config.DiscoveryEnabled() {
		t.Error("an absent ConfigMap declined the run, and it means the operator's own behavior")
	}
}

// The object exists and the key does not, which is also a read that succeeded.
func TestAConfigMapWithoutTheKeyIsNotAFailure(t *testing.T) {
	_, err := loadFrom(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: theNamespace},
		Data:       map[string]string{"something-else": "enabled: false"},
	})
	if err != nil {
		t.Fatalf("a ConfigMap without the key reported %v", err)
	}
}
