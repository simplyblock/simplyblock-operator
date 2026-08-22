package utils

import (
	"regexp"
	"strings"
	"testing"
)

// Job names are Kubernetes object names, so a node name that is long, upper-case or
// dotted must still produce a valid DNS-1123 label — and two different nodes must
// never collide, or the second node's Job would silently reuse the first's.
func TestNodeNameSuffix(t *testing.T) {
	label := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	cases := []string{
		"worker-1",
		"vm04.simplyblock4.localdomain",
		"WORKER-UPPER.example.com",
		"ip-10-0-1-23.eu-central-1.compute.internal",
		strings.Repeat("very-long-node-name", 10),
		"node_with_underscores",
		"1.2.3.4",
		"",
	}
	seen := map[string]string{}
	for _, node := range cases {
		t.Run(node, func(t *testing.T) {
			suffix := NodeNameSuffix(node)
			if !label.MatchString(suffix) {
				t.Errorf("NodeNameSuffix(%q) = %q, which is not a valid DNS-1123 label", node, suffix)
			}
			// The longest name either caller builds from these two: a prefix, a
			// migration or node UUID, and the node suffix.
			full := "vmig-validate-" + SafeNodeID("2f8c1e4a-9b3d-4c7e-8f10-5a6b7c8d9e0f") + "-" + suffix
			if len(full) > 63 {
				t.Errorf("job name %q is %d chars, over the 63-char label limit", full, len(full))
			}
			if other, dup := seen[suffix]; dup {
				t.Errorf("NodeNameSuffix(%q) collides with NodeNameSuffix(%q) = %q", node, other, suffix)
			}
			seen[suffix] = node
		})
	}

	// Same node, same suffix: the Job is recreated under the same name across
	// reconciles rather than piling up duplicates.
	if a, b := NodeNameSuffix("worker-1"), NodeNameSuffix("worker-1"); a != b {
		t.Errorf("NodeNameSuffix is not deterministic: %q vs %q", a, b)
	}
	// Hosts that share a short name but differ in domain must not collide.
	if a, b := NodeNameSuffix("vm04.dc1.example.com"), NodeNameSuffix("vm04.dc2.example.com"); a == b {
		t.Errorf("NodeNameSuffix collides for different FQDNs with the same host part: %q", a)
	}
}

// A UUID is not a label: the dashes go and the result is short enough to leave room
// for a prefix and a node suffix beside it.
func TestSafeNodeID(t *testing.T) {
	got := SafeNodeID("2f8c1e4a-9b3d-4c7e-8f10-5a6b7c8d9e0f")
	if strings.Contains(got, "-") {
		t.Errorf("SafeNodeID = %q, want no dashes", got)
	}
	if len(got) != 20 {
		t.Errorf("SafeNodeID = %q (%d chars), want it truncated to 20", got, len(got))
	}
	if short := SafeNodeID("abc"); short != "abc" {
		t.Errorf("SafeNodeID(%q) = %q, want a short id left alone", "abc", short)
	}
}
