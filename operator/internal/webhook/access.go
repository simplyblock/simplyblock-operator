// The access check a webhook runs for the user behind an admission request. The
// operator acts with wider rights than the requester once an object is admitted,
// so a reference that leaves the object's namespace is checked here.

package webhook

import (
	"context"
	"fmt"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// AccessChecker answers whether a user may do something.
type AccessChecker interface {
	Allowed(ctx context.Context, user authenticationv1.UserInfo, attrs authorizationv1.ResourceAttributes) (bool, error)
}

// reviewer asks the API server through a SubjectAccessReview.
type reviewer struct{ client client.Client }

func (r reviewer) Allowed(
	ctx context.Context, user authenticationv1.UserInfo, attrs authorizationv1.ResourceAttributes,
) (bool, error) {
	extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
	for key, values := range user.Extra {
		extra[key] = authorizationv1.ExtraValue(values)
	}
	review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: user.Username, UID: user.UID, Groups: user.Groups, Extra: extra, ResourceAttributes: &attrs,
	}}
	if err := r.client.Create(ctx, review); err != nil {
		return false, fmt.Errorf("review the access of %s: %w", user.Username, err)
	}
	return review.Status.Allowed, nil
}
