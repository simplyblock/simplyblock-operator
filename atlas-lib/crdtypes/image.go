// ImageReference: the shape every container image field in the CRDs is held
// to.
package crdtypes

// ImageReference is a container image reference from any registry, mirrors
// included: an optional registry host (a DNS name or a bracketed IPv6
// address, with an optional port), a lowercase repository path, and a tag, a
// digest, or both. The empty string is accepted so an optional field can stay
// unset. Digest pinning (@sha256:...) is recommended.
// +kubebuilder:validation:Pattern=`^($|(([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*|\[[a-fA-F0-9:]+\])(:[0-9]+)?/)?[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*(:[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}(@sha256:[a-f0-9]{64})?|@sha256:[a-f0-9]{64}))$`
type ImageReference string
