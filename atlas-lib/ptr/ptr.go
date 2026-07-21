// Package ptr provides helpers for pointers to values, which generated API
// request bodies and Kubernetes types use pervasively for optional fields.
package ptr

// To returns a pointer to v.
func To[T any](v T) *T {
	return &v
}

// From returns the value p points to, or def when p is nil.
func From[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
