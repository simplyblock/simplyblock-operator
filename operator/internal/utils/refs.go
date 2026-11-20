// Namespaced object references written as strings: "<name>" means the
// referring object's own namespace, "<namespace>/<name>" another one. They let
// a claim in a workload namespace follow a ReplicationPolicy that lives with
// the StorageCluster, and let a migration report the claims of every namespace.

package utils

import "strings"

// SplitRef returns the namespace and name a reference names, taking
// defaultNamespace for a bare name.
func SplitRef(ref, defaultNamespace string) (namespace, name string) {
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return defaultNamespace, ref
}

// CanonicalRef is the "<namespace>/<name>" form of a reference, so two
// spellings of the same object compare equal.
func CanonicalRef(ref, defaultNamespace string) string {
	ns, name := SplitRef(ref, defaultNamespace)
	return ns + "/" + name
}

// RefFrom is how an object in fromNamespace refers to namespace/name: the bare
// name within one namespace, which is what every reference was before, and
// the qualified form across namespaces.
func RefFrom(namespace, name, fromNamespace string) string {
	if namespace == fromNamespace {
		return name
	}
	return namespace + "/" + name
}
