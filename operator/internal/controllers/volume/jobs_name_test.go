package volume

import (
	"strings"
	"testing"
)

// A PersistentVolumeOps re-created under a name a failed one used must not find
// the failed one's Jobs: the Job name carries the operation's UID, so a leftover
// Job of another instance is never taken for this one's check.
func TestJobNameIsPerOperationInstance(t *testing.T) {
	node := "gke-sbcilium-default-pool-fe3ab31a-1svz"
	first := jobName(modeValidate, "mig-gke2", "f999cfea-7007-4a30-9dd0-f47ed155d00d", node)
	second := jobName(modeValidate, "mig-gke2", "bfd347fa-82a2-489f-ac45-ff736df0a177", node)
	if first == second {
		t.Fatalf("two instances of operation mig-gke2 on %s share the Job name %s", node, first)
	}
	if again := jobName(modeValidate, "mig-gke2", "f999cfea-7007-4a30-9dd0-f47ed155d00d", node); again != first {
		t.Fatalf("the name is not stable for one instance: %s then %s", first, again)
	}
	if !strings.HasPrefix(first, "pvops-validate-") {
		t.Fatalf("validate Job name %s lacks its prefix", first)
	}
	if release := jobName(modeRelease, "mig-gke2", "f999cfea-7007-4a30-9dd0-f47ed155d00d", node); !strings.HasPrefix(release, "pvops-release-") || release == first {
		t.Fatalf("release Job name %s must differ from the validate name by its prefix", release)
	}
}

// Job names are DNS labels: a long operation name on a long node name still fits.
func TestJobNameFitsALabel(t *testing.T) {
	name := jobName(modeValidate,
		"persistentvolumeops-for-pvc-0123456789abcdef0123456789abcdef",
		"f999cfea-7007-4a30-9dd0-f47ed155d00d",
		"ip-10-10-100-123.us-east-2.compute.internal")
	if len(name) > 63 {
		t.Fatalf("Job name %q is %d characters, over the 63 of a label", name, len(name))
	}
	if strings.Contains(name, "--") || strings.HasSuffix(name, "-") {
		t.Fatalf("Job name %q is not a clean label", name)
	}
}
