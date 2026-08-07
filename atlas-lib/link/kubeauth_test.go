package link

import (
	"context"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// authenticated is the TokenReview result for a bound ServiceAccount token
// issued to a CSI node plugin pod.
func authenticated(extra map[string]authenticationv1.ExtraValue) authenticationv1.TokenReviewStatus {
	return authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User: authenticationv1.UserInfo{
			Username: "system:serviceaccount:simplyblock:csi-node",
			Extra:    extra,
		},
	}
}

func boundToPod(pod, uid string) map[string]authenticationv1.ExtraValue {
	return map[string]authenticationv1.ExtraValue{
		claimPodName: {pod},
		claimPodUID:  {uid},
	}
}

// kubeClient is a fake API server that answers TokenReviews with the given
// status. The fake clientset has no TokenReview implementation of its own —
// it would echo the request back with an empty status — so the reactor is what
// makes the call mean anything.
func kubeClient(status authenticationv1.TokenReviewStatus, objects ...runtime.Object) *fake.Clientset {
	cs := fake.NewClientset(objects...)
	cs.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review, ok := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if !ok {
			return false, nil, nil
		}
		out := review.DeepCopy()
		out.Status = status
		return true, out, nil
	})
	return cs
}

func nodePod(name, uid, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock", UID: types.UID(uid)},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func TestKubeAuthenticatorTakesNodeFromTokenClaim(t *testing.T) {
	extra := boundToPod("csi-node-abc", "pod-uid-1")
	extra[claimNodeName] = authenticationv1.ExtraValue{"worker-3"}

	auth := &KubeAuthenticator{Client: kubeClient(authenticated(extra))}

	identity, err := auth.Authenticate(context.Background(), "token", Claim{ID: NodePeer("worker-3")})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.ID != NodePeer("worker-3") {
		t.Errorf("identity = %s, want node/worker-3", identity.ID)
	}
	if identity.InstanceUID != "pod-uid-1" {
		t.Errorf("instance = %q, want pod-uid-1", identity.InstanceUID)
	}
}

// A cluster whose API server does not report the node on the token review is
// still expected to work; the pod the token is bound to says where it runs.
func TestKubeAuthenticatorFallsBackToPodLookup(t *testing.T) {
	auth := &KubeAuthenticator{
		Client: kubeClient(
			authenticated(boundToPod("csi-node-abc", "pod-uid-1")),
			nodePod("csi-node-abc", "pod-uid-1", "worker-5"),
		),
	}

	identity, err := auth.Authenticate(context.Background(), "token", Claim{ID: NodePeer("worker-5")})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.ID != NodePeer("worker-5") {
		t.Errorf("identity = %s, want node/worker-5", identity.ID)
	}
}

// The attack this authenticator exists to stop: one node plugin, holding the
// DaemonSet's shared ServiceAccount token, claiming to be a different node.
func TestKubeAuthenticatorRejectsImpersonatedNode(t *testing.T) {
	extra := boundToPod("csi-node-abc", "pod-uid-1")
	extra[claimNodeName] = authenticationv1.ExtraValue{"worker-3"}

	auth := &KubeAuthenticator{Client: kubeClient(authenticated(extra))}

	_, err := auth.Authenticate(context.Background(), "token", Claim{ID: NodePeer("worker-9")})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v, want PermissionDenied", err)
	}
}

// A peer that names no node is told what it is rather than refused: the claim
// is a cross-check, not the source.
func TestKubeAuthenticatorFillsInAnUnclaimedName(t *testing.T) {
	extra := boundToPod("csi-node-abc", "pod-uid-1")
	extra[claimNodeName] = authenticationv1.ExtraValue{"worker-3"}

	auth := &KubeAuthenticator{Client: kubeClient(authenticated(extra))}

	identity, err := auth.Authenticate(context.Background(), "token", Claim{ID: PeerID{Kind: PeerKindNode}})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.ID != NodePeer("worker-3") {
		t.Errorf("identity = %s, want node/worker-3", identity.ID)
	}
}

func TestKubeAuthenticatorRejectsUnauthenticatedToken(t *testing.T) {
	auth := &KubeAuthenticator{Client: kubeClient(authenticationv1.TokenReviewStatus{
		Authenticated: false,
		Error:         "token expired",
	})}

	_, err := auth.Authenticate(context.Background(), "token", Claim{ID: NodePeer("worker-3")})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
}

// An unbound token names no pod, so there is nothing to derive an identity
// from — only the peer's word for it, which is what must not be trusted.
func TestKubeAuthenticatorRejectsUnboundToken(t *testing.T) {
	auth := &KubeAuthenticator{Client: kubeClient(authenticated(nil))}

	_, err := auth.Authenticate(context.Background(), "token", Claim{ID: NodePeer("worker-3")})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v, want PermissionDenied", err)
	}
}

func TestKubeAuthenticatorRejectsNonServiceAccount(t *testing.T) {
	auth := &KubeAuthenticator{Client: kubeClient(authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: "kubernetes-admin"},
	})}

	_, err := auth.Authenticate(context.Background(), "token", Claim{ID: NodePeer("worker-3")})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v, want PermissionDenied", err)
	}
}

func TestKubeAuthenticatorRestrictsKindsByServiceAccount(t *testing.T) {
	extra := boundToPod("csi-node-abc", "pod-uid-1")
	extra[claimNodeName] = authenticationv1.ExtraValue{"worker-3"}
	client := kubeClient(authenticated(extra))

	auth := &KubeAuthenticator{
		Client: client,
		ServiceAccounts: map[PeerKind][]string{
			PeerKindNode:       {"simplyblock/csi-node"},
			PeerKindController: {"simplyblock/csi-controller"},
		},
	}

	if _, err := auth.Authenticate(context.Background(), "token", Claim{ID: NodePeer("worker-3")}); err != nil {
		t.Fatalf("node plugin registering as a node: %v", err)
	}

	// The same token asking for the controller's place in the registry.
	_, err := auth.Authenticate(context.Background(), "token", Claim{ID: ControllerPeer("csi-node-abc")})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v, want PermissionDenied", err)
	}
}

func TestKubeAuthenticatorNamesAControllerByItsPod(t *testing.T) {
	auth := &KubeAuthenticator{Client: kubeClient(authenticated(boundToPod("csi-controller-xyz", "pod-uid-7")))}

	identity, err := auth.Authenticate(context.Background(), "token",
		Claim{ID: ControllerPeer("csi-controller-xyz")})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.ID != ControllerPeer("csi-controller-xyz") {
		t.Errorf("identity = %s, want controller/csi-controller-xyz", identity.ID)
	}
}

// An API server that cannot be reached says nothing about the token, so the
// peer must be told to come back rather than that it was rejected.
func TestKubeAuthenticatorTreatsReviewFailureAsRetryable(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})

	auth := &KubeAuthenticator{Client: cs}

	_, err := auth.Authenticate(context.Background(), "token", Claim{ID: NodePeer("worker-3")})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want Unavailable", err)
	}
}

func TestKubeAuthenticatorPassesAudiences(t *testing.T) {
	var got []string
	cs := fake.NewClientset()
	cs.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		got = review.Spec.Audiences
		out := review.DeepCopy()
		extra := boundToPod("csi-node-abc", "pod-uid-1")
		extra[claimNodeName] = authenticationv1.ExtraValue{"worker-3"}
		out.Status = authenticated(extra)
		return true, out, nil
	})

	auth := &KubeAuthenticator{Client: cs, Audiences: []string{"atlas-link"}}
	if _, err := auth.Authenticate(context.Background(), "token", Claim{ID: NodePeer("worker-3")}); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(got) != 1 || got[0] != "atlas-link" {
		t.Errorf("audiences sent = %v, want [atlas-link]", got)
	}
}