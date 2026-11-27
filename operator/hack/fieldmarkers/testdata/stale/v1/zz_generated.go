// A stale generated file for the fixture in types.go: it still copies a field
// the type no longer has, so the package carries a type error.

package v1

func (in *GizmoSpec) DeepCopyInto(out *GizmoSpec) {
	*out = *in
	out.Removed = in.Removed
}
