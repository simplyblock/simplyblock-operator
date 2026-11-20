// Package ownimage derives the images the operator deploys from the operator's
// own image reference.
//
// Every image the operator ships beside itself (the CSI driver, the pNFS
// metadata server, and the rebalancer the migration Jobs run) is built from the
// same commit and pushed to the same registries under the same tag. So the
// operator's own registry and tag, with another repository, is the image of the
// same release. Deriving them here keeps the controllers from each carrying a
// literal default, and a literal default is always some other release.
//
// The operator's reference comes from SB_OPERATOR_IMAGE rather than from its
// own pod. A pod's spec names whatever tag it was deployed with, and a registry
// may have moved that tag since.
package ownimage

import (
	"fmt"
	"os"
	"strings"
)

// Env names the operator's own image. The chart and the Kustomize installer
// set it to exactly what they deploy the manager from.
const Env = "SB_OPERATOR_IMAGE"

// SiblingOfOperator is repository at the operator's own registry and tag.
//
// It fails when Env is unset or names no tag, and the error says which, so a
// caller can wrap it with what the image was for.
func SiblingOfOperator(repository string) (string, error) {
	operator := os.Getenv(Env)
	if operator == "" {
		return "", fmt.Errorf("%s is unset", Env)
	}
	sibling, err := Sibling(operator, repository)
	if err != nil {
		return "", fmt.Errorf("%s=%q cannot be turned into a %s image: %w", Env, operator, repository, err)
	}
	return sibling, nil
}

// Sibling rewrites a reference to name a different repository in the same
// registry at the same tag.
//
// A digest is dropped rather than carried, because a digest identifies one
// image and says nothing about another. A reference that carries only a digest
// has no tag to reuse and is refused, since guessing one would deploy something
// nobody named.
func Sibling(reference, repository string) (string, error) {
	withoutDigest, _, _ := strings.Cut(reference, "@")

	// The tag is what follows the last colon, and only when that colon comes
	// after the last slash. A registry may carry a port, and `host:5000/repo`
	// has a colon that is not a tag separator.
	slash := strings.LastIndex(withoutDigest, "/")
	colon := strings.LastIndex(withoutDigest, ":")
	if colon < 0 || colon < slash {
		return "", fmt.Errorf("it names no tag")
	}
	base, tag := withoutDigest[:colon], withoutDigest[colon+1:]
	if tag == "" {
		return "", fmt.Errorf("it names no tag")
	}
	if slash < 0 {
		return "", fmt.Errorf("it names no registry")
	}

	return base[:slash+1] + repository + ":" + tag, nil
}
