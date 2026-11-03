// The add-on images, for air-gapped installs: every one is pinned by digest in
// the embedded manifests, listed in related-images.yaml for mirroring, and
// rewritten to a mirror registry when one is configured.

package prerequisites

import (
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// InstallOptions adjusts what Install applies.
type InstallOptions struct {
	// ImageRegistry replaces the registry host of every image, keeping its
	// path, tag, and digest. Empty keeps the upstream registries.
	ImageRegistry string

	// Namespace is where a namespaced add-on is installed instead of its
	// upstream namespace. Empty keeps the upstream one.
	Namespace string
}

var pinnedImage = regexp.MustCompile(`(?:image: "?|-image=)([^"\s]+@sha256:[0-9a-f]{64})`)

// Images lists every image the embedded manifests use, sorted and unique.
func Images() []string {
	var out []string
	for _, p := range All() {
		raw, err := manifests.ReadFile(p.file)
		if err != nil {
			panic(err) // the embedded set is fixed at build time
		}
		for _, m := range pinnedImage.FindAllStringSubmatch(string(raw), -1) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Mirror moves ref to registry, keeping everything after the host. An empty
// registry returns ref unchanged.
func Mirror(ref, registry string) string {
	if registry == "" {
		return ref
	}
	_, rest, _ := strings.Cut(ref, "/")
	return strings.TrimSuffix(registry, "/") + "/" + rest
}

// rewriteImages replaces every image reference in every string of obj,
// which covers container images, flags, and images embedded in ConfigMap data.
func rewriteImages(obj *unstructured.Unstructured, registry string) {
	if registry == "" {
		return
	}
	replacer := make([]string, 0, 2*len(Images()))
	for _, ref := range Images() {
		replacer = append(replacer, ref, Mirror(ref, registry))
	}
	r := strings.NewReplacer(replacer...)
	obj.Object = rewriteValue(obj.Object, r).(map[string]any)
}

func rewriteValue(v any, r *strings.Replacer) any {
	switch t := v.(type) {
	case string:
		return r.Replace(t)
	case map[string]any:
		for k, e := range t {
			t[k] = rewriteValue(e, r)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = rewriteValue(e, r)
		}
		return t
	default:
		return v
	}
}
