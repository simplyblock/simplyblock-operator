package link

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Claims the API server puts on a bound ServiceAccount token's TokenReview
// result, naming the pod the token was issued to and the node that pod runs on.
// They are what make a shared ServiceAccount usable as per-node identity.
const (
	claimPodName  = "authentication.kubernetes.io/pod-name"
	claimPodUID   = "authentication.kubernetes.io/pod-uid"
	claimNodeName = "authentication.kubernetes.io/node-name"
)

// serviceAccountPrefix is how the API server spells a ServiceAccount subject:
// system:serviceaccount:<namespace>:<name>.
const serviceAccountPrefix = "system:serviceaccount:"

// KubeAuthenticator verifies a peer's Kubernetes ServiceAccount token and
// derives its identity from what that token is bound to.
//
// The distinction it exists to enforce: a CSI node plugin's token proves the
// caller is *a* node plugin, not *which* node it runs on — the whole DaemonSet
// shares one ServiceAccount. A hub that believed the node name a peer sent
// would let any compromised node register as any other and answer for its
// volumes. So the node name comes from the token's own binding: the API server
// reports the pod the token was issued to and, on clusters that support it, the
// node that pod is on. A Hello whose claim contradicts that is refused rather
// than corrected.
//
// The zero value is not usable; Client is required.
type KubeAuthenticator struct {
	// Client talks to the API server. It needs create on tokenreviews, and
	// get on pods when NodeFromPod is in play.
	Client kubernetes.Interface

	// Audiences, when set, is the audience the token must have been issued
	// for. Project a token with a dedicated audience for the link and set it
	// here: a token bound to nothing in particular is one that any component
	// holding it can replay against this hub.
	Audiences []string

	// ServiceAccounts, when non-empty, restricts which ServiceAccounts may
	// register as which kind, as "namespace/name". Without it any
	// authenticated ServiceAccount may register as any kind — its *name* is
	// still verified, so it cannot impersonate another peer, but a node plugin
	// could register as the controller and take its place in the registry.
	ServiceAccounts map[PeerKind][]string
}

// Authenticate verifies the token and returns the identity it is bound to.
func (a *KubeAuthenticator) Authenticate(ctx context.Context, token string, claim Claim) (Identity, error) {
	if a.Client == nil {
		return Identity{}, status.Error(codes.Internal, "link: authenticator has no Kubernetes client")
	}
	if claim.ID.Kind == "" {
		return Identity{}, status.Error(codes.InvalidArgument, "link: peer claimed no kind")
	}

	review, err := a.Client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: a.Audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		// The API server could not be asked, which says nothing about the
		// token. Unavailable so the peer retries rather than treating its
		// perfectly good credentials as rejected.
		return Identity{}, status.Errorf(codes.Unavailable, "link: token review: %v", err)
	}
	if !review.Status.Authenticated {
		reason := review.Status.Error
		if reason == "" {
			reason = "token not authenticated"
		}
		return Identity{}, status.Errorf(codes.Unauthenticated, "link: %s", reason)
	}

	namespace, account, err := splitServiceAccount(review.Status.User.Username)
	if err != nil {
		return Identity{}, status.Errorf(codes.PermissionDenied, "link: %v", err)
	}
	if err := a.authorizeKind(claim.ID.Kind, namespace, account); err != nil {
		return Identity{}, err
	}

	extra := review.Status.User.Extra
	podName := firstExtra(extra, claimPodName)
	podUID := firstExtra(extra, claimPodUID)
	if podName == "" {
		// An unbound (legacy or manually minted) token names no pod, so there
		// is nothing to derive an identity from — only something to take the
		// peer's word for, which is the thing this authenticator exists to
		// avoid.
		return Identity{}, status.Error(codes.PermissionDenied,
			"link: token is not bound to a pod; project a bound ServiceAccount token")
	}

	name, err := a.verifiedName(ctx, claim.ID.Kind, namespace, podName, extra)
	if err != nil {
		return Identity{}, err
	}
	if claim.ID.Name != "" && claim.ID.Name != name {
		return Identity{}, status.Errorf(codes.PermissionDenied,
			"link: peer claimed to be %s but its credentials say %s",
			PeerID{Kind: claim.ID.Kind, Name: claim.ID.Name}, PeerID{Kind: claim.ID.Kind, Name: name})
	}

	// The pod UID is the verified process lifetime. Falling back to the claim
	// is safe where the identity itself is not: it only decides which of two
	// sessions for the same verified peer supersedes the other.
	instance := podUID
	if instance == "" {
		instance = claim.InstanceUID
	}
	return Identity{ID: PeerID{Kind: claim.ID.Kind, Name: name}, InstanceUID: instance}, nil
}

// verifiedName resolves the peer's name for its kind: the node it runs on for a
// node plugin, its own pod for anything else.
func (a *KubeAuthenticator) verifiedName(
	ctx context.Context,
	kind PeerKind,
	namespace, podName string,
	extra map[string]authenticationv1.ExtraValue,
) (string, error) {
	if kind != PeerKindNode {
		return podName, nil
	}

	// Recent API servers report the node directly on the token review, which
	// makes this a single call. Older ones do not, so fall back to reading the
	// pod the token is bound to — same answer, one more round trip and a get
	// on pods in the hub's RBAC.
	if node := firstExtra(extra, claimNodeName); node != "" {
		return node, nil
	}

	pod, err := a.Client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", status.Errorf(codes.Unavailable, "link: reading pod %s/%s: %v", namespace, podName, err)
	}
	if pod.Spec.NodeName == "" {
		return "", status.Errorf(codes.FailedPrecondition,
			"link: pod %s/%s is not scheduled to a node yet", namespace, podName)
	}
	return pod.Spec.NodeName, nil
}

// authorizeKind checks the ServiceAccount against the kinds it may register as.
func (a *KubeAuthenticator) authorizeKind(kind PeerKind, namespace, account string) error {
	allowed, restricted := a.ServiceAccounts[kind]
	if !restricted || len(allowed) == 0 {
		if len(a.ServiceAccounts) == 0 {
			return nil // no restrictions configured at all
		}
		return status.Errorf(codes.PermissionDenied, "link: no ServiceAccount may register as kind %q", kind)
	}
	if slices.Contains(allowed, namespace+"/"+account) {
		return nil
	}
	return status.Errorf(codes.PermissionDenied,
		"link: ServiceAccount %s/%s may not register as kind %q", namespace, account, kind)
}

// splitServiceAccount picks the namespace and name out of a ServiceAccount
// subject, and rejects any subject that is not one — a user or a node
// certificate has no pod binding to derive an identity from.
func splitServiceAccount(username string) (namespace, name string, err error) {
	rest, ok := strings.CutPrefix(username, serviceAccountPrefix)
	if !ok {
		return "", "", fmt.Errorf("subject %q is not a ServiceAccount", username)
	}
	namespace, name, ok = strings.Cut(rest, ":")
	if !ok || namespace == "" || name == "" {
		return "", "", fmt.Errorf("malformed ServiceAccount subject %q", username)
	}
	return namespace, name, nil
}

func firstExtra(extra map[string]authenticationv1.ExtraValue, key string) string {
	if values := extra[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

// readTrimmedFile reads a file and trims the whitespace around it, which token
// and identity files routinely carry a trailing newline of.
func readTrimmedFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}