// The image both plugins run, and where it comes from when the spec is silent.
//
// The default is the pairing every release is tested as: the operator's own
// registry and tag, with the CSI driver's repository. A deployment that states
// nothing therefore follows the operator it is running under.

package driver

import (
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
)

// imagePattern is the pattern the generated CRD enforces on spec.image,
// read out of the CRD rather than restated, so that a change to one is a
// failure here rather than a divergence nobody notices.
var imagePattern = func() *regexp.Regexp {
	f, err := os.Open("../../../config/crd/bases/storage.simplyblock.io_simplyblockdrivers.yaml")
	if err != nil {
		panic(err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(f)
	if err != nil {
		panic(err)
	}
	// The first pattern under the spec's own image property.
	i := strings.Index(string(body), "              image:")
	rest := string(body)[i:]
	j := strings.Index(rest, "pattern: ")
	line := rest[j+len("pattern: "):]
	line = line[:strings.IndexByte(line, '\n')]
	return regexp.MustCompile(strings.TrimSpace(line))
}()

func TestDriverImagePrefersTheSpec(t *testing.T) {
	t.Setenv(OperatorImageEnv, "quay.io/simplyblock-io/simplyblock-operator:v26.2.6")

	d := testDriver("simplyblock")
	d.Spec.Image = "public.ecr.aws/simply-block/spdkcsi:some-branch"

	got, err := driverImage(d)
	if err != nil {
		t.Fatalf("driverImage: %v", err)
	}
	if got != d.Spec.Image {
		t.Errorf("image = %q, want the spec's %q", got, d.Spec.Image)
	}
}

// The registry and the tag are the operator's, and only the repository changes.
func TestDriverImageDefaultsToTheOperatorsBuild(t *testing.T) {
	tests := []struct {
		name     string
		operator string
		want     string
	}{
		{
			name:     "a release tag",
			operator: "quay.io/simplyblock-io/simplyblock-operator:v26.2.6",
			want:     "quay.io/simplyblock-io/spdkcsi:v26.2.6",
		},
		{
			name:     "a branch build, which is what a development cluster runs",
			operator: "public.ecr.aws/simply-block/simplyblock-operator:feat-simplyblockdriver",
			want:     "public.ecr.aws/simply-block/spdkcsi:feat-simplyblockdriver",
		},
		{
			// A digest identifies one image and says nothing about another, so
			// it is dropped and the tag beside it is what carries over.
			name:     "a tag with a digest pinned beside it",
			operator: "quay.io/simplyblock-io/simplyblock-operator:v26.2.6@sha256:" + strings64(),
			want:     "quay.io/simplyblock-io/spdkcsi:v26.2.6",
		},
		{
			name:     "a registry carrying a port",
			operator: "registry.internal:5000/simplyblock/simplyblock-operator:v1",
			want:     "registry.internal:5000/simplyblock/spdkcsi:v1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(OperatorImageEnv, tc.operator)

			d := testDriver("simplyblock")
			d.Spec.Image = ""

			got, err := driverImage(d)
			if err != nil {
				t.Fatalf("driverImage: %v", err)
			}
			if got != tc.want {
				t.Errorf("image = %q, want %q", got, tc.want)
			}
		})
	}
}

// Where neither the spec nor the environment answers, the error says which of
// the two to set rather than leaving a plugin that cannot pull.
func TestDriverImageWithoutAnAnswer(t *testing.T) {
	tests := []struct {
		name     string
		operator string
	}{
		{name: "nothing set the operator's image", operator: ""},
		{name: "the operator's image names no tag", operator: "quay.io/simplyblock-io/simplyblock-operator"},
		{
			name:     "the operator's image is pinned by digest alone",
			operator: "quay.io/simplyblock-io/simplyblock-operator@sha256:" + strings64(),
		},
		{name: "the operator's image names no registry", operator: "simplyblock-operator:v1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(OperatorImageEnv, tc.operator)

			d := testDriver("simplyblock")
			d.Spec.Image = ""

			got, err := driverImage(d)
			if err == nil {
				t.Fatalf("image = %q, want an error naming what to set", got)
			}
			if !contains(err.Error(), OperatorImageEnv) && !contains(err.Error(), "spec.image") {
				t.Errorf("the error names neither the field nor the variable: %v", err)
			}
		})
	}
}

// The derived default has to satisfy the CRD's own registry pattern, or an
// object the operator defaulted would be one admission rejects on the next
// edit.
func TestTheDefaultSatisfiesTheRegistryPattern(t *testing.T) {
	for _, operator := range []string{
		"quay.io/simplyblock-io/simplyblock-operator:v26.2.6",
		"public.ecr.aws/simply-block/simplyblock-operator:feat-simplyblockdriver",
		"docker.io/simplyblock/simplyblock-operator:v1",
	} {
		t.Setenv(OperatorImageEnv, operator)

		d := testDriver("simplyblock")
		d.Spec.Image = ""

		got, err := driverImage(d)
		if err != nil {
			t.Fatalf("driverImage: %v", err)
		}
		if !imagePattern.MatchString(got) {
			t.Errorf("the default %q does not satisfy the pattern the CRD enforces", got)
		}
	}
}

func strings64() string {
	const hex = "0123456789abcdef"
	out := make([]byte, 64)
	for i := range out {
		out[i] = hex[i%len(hex)]
	}
	return string(out)
}
