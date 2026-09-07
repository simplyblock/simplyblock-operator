// What a pod name derived from a node or an image has to hold up to.
//
// The name is built rather than chosen, from strings that carry characters
// Kubernetes does not accept in a name: a node's dots, and an image
// reference's slashes, colons, and — when it is pinned rather than tagged —
// the at sign of a digest. A name that fails validation surfaces as a refused
// Apply, at the point of starting a shell, which reads as the cluster being
// wrong rather than the name being.

package fabric

import (
	"regexp"
	"strings"
	"testing"
)

// dns1123Subdomain is what Kubernetes accepts as an object name: lower-case
// alphanumerics, dashes, and dots, starting and ending alphanumeric.
var dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func TestSanitizeProducesAUsableName(t *testing.T) {
	for name, in := range map[string]string{
		"a node":                    "sbi-99984-controlplane-1",
		"a node with dots":          "vm01.simplyblock4.localdomain",
		"an image with a tag":       "quay.io/simplyblock-io/spdkcsi:volstack-integration",
		"an image pinned by digest": "quay.io/simplyblock-io/spdkcsi@sha256:3f2b1c00beef4000800000000000beef3f2b1c00beef4000800000000000beef",
		"an image with a port":      "registry.local:5000/simplyblock/spdkcsi:latest",
		"mixed case":                "Quay.IO/Simplyblock/SPDKCSI:Latest",
		"underscores":               "some_node_name",
	} {
		t.Run(name, func(t *testing.T) {
			got := "sb-shell-" + sanitize(in)
			if !dns1123Subdomain.MatchString(got) {
				t.Errorf("sanitize(%q) makes %q, which Kubernetes will refuse as a name", in, got)
			}
			if len(got) > 253 {
				t.Errorf("sanitize(%q) makes a %d character name, past the 253 a name may be", in, len(got))
			}
		})
	}
}

// TestSanitizeKeepsNamesApart is what the sanitizing is for besides validity:
// two shells that differ only in their image must not land on one pod name,
// since the second Apply would try to mutate the immutable image of the first.
func TestSanitizeKeepsNamesApart(t *testing.T) {
	tagged := sanitize("quay.io/simplyblock-io/spdkcsi:volstack-integration")
	pinned := sanitize("quay.io/simplyblock-io/spdkcsi@sha256:3f2b1c00beef")
	if tagged == pinned {
		t.Errorf("a tagged and a pinned image collapse to one name: %q", tagged)
	}
	if strings.Contains(pinned, "@") || strings.Contains(pinned, ":") || strings.Contains(pinned, "/") {
		t.Errorf("%q still carries a character a name may not hold", pinned)
	}
}
