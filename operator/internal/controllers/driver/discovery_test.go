// The detection itself: what a cluster without the snapshot CRDs says, and what
// is done with an answer that is neither yes nor no.

package driver

import (
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// answeringDiscovery is a discovery client that returns one answer, and counts
// how often it was asked.
type answeringDiscovery struct {
	discovery.DiscoveryInterface
	err   error
	asked int
}

func (a *answeringDiscovery) ServerResourcesForGroupVersion(
	string,
) (*metav1.APIResourceList, error) {
	a.asked++
	if a.err != nil {
		return nil, a.err
	}
	return &metav1.APIResourceList{}, nil
}

// A cluster without the snapshot CRDs answers NotFound for the group version,
// and that is the answer rather than a failure: it is what "this cluster has no
// snapshot support" looks like over discovery.
func TestAGroupThatIsNotServedIsAnAnswerRatherThanAnError(t *testing.T) {
	api := &DiscoveredSnapshotAPI{Discovery: &answeringDiscovery{
		err: apierrors.NewNotFound(schema.GroupResource{Group: snapshotGroupVersion.Group}, ""),
	}}

	served, err := api.SnapshotAPIServed(t.Context())
	if err != nil {
		t.Fatalf("a cluster with no snapshot CRDs reported an error: %v", err)
	}
	if served {
		t.Error("a cluster with no snapshot CRDs was reported as serving the API")
	}
}

func TestAServedGroupIsReported(t *testing.T) {
	api := &DiscoveredSnapshotAPI{Discovery: &answeringDiscovery{}}

	served, err := api.SnapshotAPIServed(t.Context())
	if err != nil {
		t.Fatalf("asking: %v", err)
	}
	if !served {
		t.Error("a cluster serving the snapshot API was reported as not serving it")
	}
}

// Anything that is not an answer is returned. A reconcile that guessed here
// would either apply a class the cluster has no kind for or withdraw one an
// adopted cluster is using.
func TestAnUnreachableAPIServerIsReported(t *testing.T) {
	api := &DiscoveredSnapshotAPI{Discovery: &answeringDiscovery{
		err: errors.New("connection refused"),
	}}

	if _, err := api.SnapshotAPIServed(t.Context()); err == nil {
		t.Error("an unreachable API server was read as an answer about the snapshot API")
	}
}

// The answer is cached, because discovery is a round trip and the set of served
// groups changes when somebody installs a CRD rather than continuously.
func TestTheAnswerIsCachedAndThenAskedAgain(t *testing.T) {
	clock := time.Now()
	discovered := &answeringDiscovery{}
	api := &DiscoveredSnapshotAPI{Discovery: discovered, nowFuncT: func() time.Time { return clock }}

	for range 3 {
		if _, err := api.SnapshotAPIServed(t.Context()); err != nil {
			t.Fatalf("asking: %v", err)
		}
	}
	if discovered.asked != 1 {
		t.Errorf("asked the API server %d times within the window, want once", discovered.asked)
	}

	clock = clock.Add(snapshotAPITTL + time.Second)
	if _, err := api.SnapshotAPIServed(t.Context()); err != nil {
		t.Fatalf("asking: %v", err)
	}
	if discovered.asked != 2 {
		t.Errorf("asked %d times after the window expired, want the question to be asked "+
			"again so a cluster that gains snapshot support is noticed", discovered.asked)
	}
}

// The zero value refuses rather than reporting a cluster with no snapshot
// support, which would be indistinguishable from an answer.
func TestNoDiscoveryClientRefuses(t *testing.T) {
	var api DiscoveredSnapshotAPI
	if _, err := api.SnapshotAPIServed(t.Context()); err == nil {
		t.Error("a detector with no client answered the question")
	}
}
