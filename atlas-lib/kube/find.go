package kube

import "github.com/simplyblock/atlas/errs"

// FindByKey returns the single item in items for which keyFn(item) equals
// key, or errs.ErrNotFound if none matches. It is the generic linear-scan
// mechanism behind resolving one object out of a list by a field the caller
// controls the meaning of (a backend UUID, a label, a name) — this package
// has no concrete knowledge of the object type. A caller lists the objects
// itself (e.g. via a controller-runtime client.Client, a CRD's own generated
// clientset, or a client-go lister) and supplies keyFn; FindByKey does the
// rest, so every such lookup shares one not-found convention instead of each
// call site inventing its own.
func FindByKey[T any](items []T, keyFn func(T) string, key string) (T, error) {
	var zero T
	for i := range items {
		if keyFn(items[i]) == key {
			return items[i], nil
		}
	}
	return zero, errs.ErrNotFound
}
