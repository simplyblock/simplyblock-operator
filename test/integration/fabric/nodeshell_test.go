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

	"gopkg.in/yaml.v3"
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

// TestManifestSharedHostDir checks the shared directory lands in the pod spec
// where Kubernetes reads it, and that a shell without one is the pod it always
// was. The manifest is a template, so a wrong indent is valid text and an
// invalid pod: it would surface as a refused Apply on a live cluster.
func TestManifestSharedHostDir(t *testing.T) {
	type pod struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Containers []struct {
				VolumeMounts []struct {
					Name             string `yaml:"name"`
					MountPath        string `yaml:"mountPath"`
					MountPropagation string `yaml:"mountPropagation"`
				} `yaml:"volumeMounts"`
			} `yaml:"containers"`
			Volumes []struct {
				Name     string `yaml:"name"`
				HostPath struct {
					Path string `yaml:"path"`
					Type string `yaml:"type"`
				} `yaml:"hostPath"`
			} `yaml:"volumes"`
		} `yaml:"spec"`
	}
	podOf := func(t *testing.T, manifest string) pod {
		t.Helper()
		dec := yaml.NewDecoder(strings.NewReader(manifest))
		for {
			var p pod
			if err := dec.Decode(&p); err != nil {
				t.Fatalf("no Pod in the manifest: %v\n%s", err, manifest)
			}
			if p.Kind == "Pod" {
				return p
			}
		}
	}

	t.Run("without one, the pod is unchanged", func(t *testing.T) {
		s := &Shell{pod: "p", node: "n", image: defaultShellImage}
		p := podOf(t, s.manifest())
		if got := len(p.Spec.Volumes); got != 3 {
			t.Fatalf("%d volumes, want the 3 every shell has", got)
		}
		if strings.Contains(s.manifest(), "Bidirectional") {
			t.Fatal("a shell without a shared directory propagates a mount")
		}
	})

	t.Run("with one, it is mounted at the same path and propagates both ways", func(t *testing.T) {
		s := &Shell{pod: "p", node: "n", image: defaultShellImage, sharedDir: "/var/mnt/pnfs"}
		p := podOf(t, s.manifest())
		mounts := p.Spec.Containers[0].VolumeMounts
		last := mounts[len(mounts)-1]
		if last.Name != "shared" || last.MountPath != "/var/mnt/pnfs" || last.MountPropagation != "Bidirectional" {
			t.Errorf("shared mount = %+v, want /var/mnt/pnfs with Bidirectional propagation", last)
		}
		vol := p.Spec.Volumes[len(p.Spec.Volumes)-1]
		if vol.Name != "shared" || vol.HostPath.Path != "/var/mnt/pnfs" || vol.HostPath.Type != "DirectoryOrCreate" {
			t.Errorf("shared volume = %+v, want hostPath /var/mnt/pnfs, DirectoryOrCreate", vol)
		}
	})
}
