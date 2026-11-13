// The OpenAPI model package of the metrics group. openapi-gen reads package tags
// from a file named doc.go and nowhere else, so this file exists to carry the tag.
// The package documentation stays in groupversion_info.go.
//
// The tag gives every type an OpenAPI name in reverse-domain form. Without it a
// type is named by its Go import path, and the slashes in a reference to a nested
// type such as LogicalVolumeCapacity are escaped in a way the kube-openapi proto
// parser does not resolve. The kube-apiserver merges this group's document into
// the cluster's /openapi/v2, so every client that parses that document fails,
// Argo CD among them.
//
// +k8s:openapi-model-package=com.github.simplyblock.simplyblock-operator.api.metrics.v1alpha2

package v1alpha2
