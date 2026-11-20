// Package crdtypes holds the validated value types that custom resource
// fields share across API groups and versions, so a validation rule is
// written once.
//
// A type here carries its kubebuilder validation markers on the type itself.
// controller-gen copies them into the schema of every field of that type, in
// whichever consumer's CRDs the field appears. The package is plain Go: it
// imports nothing from Kubernetes and needs no generated deepcopy, because
// every type is a named scalar.
package crdtypes
