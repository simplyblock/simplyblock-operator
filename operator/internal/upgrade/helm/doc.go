// Package helm drives Helm through its Go SDK, so that neither the Helm binary
// nor kubectl has to be on the machine running an upgrade.
//
// The SDK is a library: it renders charts and performs releases in process,
// over client-go, and needs nothing installed. What it does bring is its own
// idea of which cluster to talk to, and that is what this package exists to
// prevent. §29.4 has the tool reading a kubeconfig the user named, and a Helm
// configuration built the usual way would re-resolve KUBECONFIG and the current
// context from the environment instead, so an upgrade could inspect one cluster
// and release to another.
//
// Everything here therefore takes the connection the rest of the run already
// holds, and nothing here reads the environment.
package helm
