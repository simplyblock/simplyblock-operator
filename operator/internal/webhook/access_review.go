// The question an admission webhook asks the API server about the person whose
// request it is judging: may this identity do that, there. It lives here because
// more than one validator asks it, and because the answer has to come from the
// API server's own authorizer. A webhook that reimplemented RBAC would disagree
// with it the first time a cluster used a webhook authorizer or an aggregated
// role.

package webhook

import (
	"context"
	"fmt"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Creating a SubjectAccessReview asks the authorizer a question and stores
// nothing. It grants no access to any object, which is why it is not an
// escalation primitive, and the StorageBackupOps validator needs it to check a
// requester's own permissions on a namespace the operation refers to.
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// AccessReviewer answers whether an identity may perform one action on one
// resource. It is an interface so that a validator's tests can say what the
// API server would answer without a running one.
type AccessReviewer interface {
	Allowed(
		ctx context.Context,
		user authenticationv1.UserInfo,
		attributes authorizationv1.ResourceAttributes,
	) (bool, error)
}

// SubjectAccessReviewer asks the API server through a SubjectAccessReview, which
// is how the authorizer is consulted for an identity other than the caller's own.
type SubjectAccessReviewer struct {
	Client client.Client
}

// Allowed implements AccessReviewer. The review carries the user, groups, UID,
// and extra attributes exactly as the admission request did, because the
// authorizer decides on all of them.
func (r *SubjectAccessReviewer) Allowed(
	ctx context.Context,
	user authenticationv1.UserInfo,
	attributes authorizationv1.ResourceAttributes,
) (bool, error) {
	extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
	for key, values := range user.Extra {
		extra[key] = authorizationv1.ExtraValue(values)
	}
	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:               user.Username,
			Groups:             user.Groups,
			UID:                user.UID,
			Extra:              extra,
			ResourceAttributes: &attributes,
		},
	}
	if err := r.Client.Create(ctx, review); err != nil {
		return false, fmt.Errorf("ask whether %q may %s %s: %w",
			user.Username, attributes.Verb, attributes.Resource, err)
	}
	return review.Status.Allowed, nil
}
