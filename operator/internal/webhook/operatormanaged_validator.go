// The operator-managed-fields validator: one handler for every kind whose spec
// carries a field marked +simplyblock:operatorManaged. It lives with the other
// admission handlers, and reads the table hack/fieldmarkers generates into
// zz_generated.operatormanaged.go, so a marker on an API type is all it takes
// to guard a field.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// OperatorManagedField is one guarded field: its path as the annotations spell
// it, its JSON segments ("*" for the values of a map), and the writers besides
// the operator that may change it.
type OperatorManagedField struct {
	Path     string
	Segments []string
	Writers  []string
}

// OperatorManagedValidator refuses an update that changes an operator-managed
// field unless it comes from the operator or from a writer the field admits.
//
// A create is admitted: it states a field for the first time, and a StorageNode
// written by hand has to name its worker. A CEL rule cannot express this guard,
// because it cannot see who sent the request, and an immutability rule would
// refuse the operator as well.
type OperatorManagedValidator struct {
	// Scheme decodes an object into its Go type and back, so a field omitted and
	// a field sent as its zero value compare equal.
	Scheme *runtime.Scheme
	// OperatorUsername is the operator's own service-account username. It is
	// matched exactly: other workloads share the operator's namespace.
	OperatorUsername string
	// Writers maps each writer a marker may name (hub, for one) to the usernames
	// it stands for.
	Writers map[string][]string
	// ResolveWriter returns the usernames of a writer that is known only at
	// request time, such as the CSI controller, whose service account is named
	// after the SimplyblockDriver. It is consulted for a writer Writers lacks.
	ResolveWriter func(ctx context.Context, writer string) ([]string, error)
	// Fields is the table of guarded fields; nil means the generated one.
	Fields map[schema.GroupVersionKind][]OperatorManagedField
}

func (v *OperatorManagedValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Update {
		return admission.Allowed("")
	}
	gvk := schema.GroupVersionKind{Group: req.Kind.Group, Version: req.Kind.Version, Kind: req.Kind.Kind}
	table := v.Fields
	if table == nil {
		table = generatedOperatorManagedFields
	}
	fields := table[gvk]
	if len(fields) == 0 {
		return admission.Allowed("")
	}

	oldObj, err := v.normalize(gvk, req.OldObject.Raw)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode the stored object: %w", err))
	}
	newObj, err := v.normalize(gvk, req.Object.Raw)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode the object: %w", err))
	}

	user := req.UserInfo.Username
	var refused []string
	for _, f := range fields {
		if reflect.DeepEqual(project(oldObj, f.Segments), project(newObj, f.Segments)) {
			continue
		}
		allowed, err := v.mayWrite(ctx, user, f.Writers)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		if !allowed {
			refused = append(refused, f.Path+" ("+writtenBy(f.Writers)+")")
		}
	}
	if len(refused) == 0 {
		return admission.Allowed("")
	}
	return admission.Denied(fmt.Sprintf(
		"%q may not change operator-managed fields (+simplyblock:operatorManaged): %s",
		user, strings.Join(refused, ", ")))
}

// mayWrite reports whether user is the operator or one of the named writers.
func (v *OperatorManagedValidator) mayWrite(ctx context.Context, user string, writers []string) (bool, error) {
	if user == v.OperatorUsername && user != "" {
		return true, nil
	}
	for _, w := range writers {
		usernames, static := v.Writers[w]
		if !static && v.ResolveWriter != nil {
			resolved, err := v.ResolveWriter(ctx, w)
			if err != nil {
				return false, fmt.Errorf("resolve the %s writer: %w", w, err)
			}
			usernames = resolved
		}
		if slices.Contains(usernames, user) {
			return true, nil
		}
	}
	return false, nil
}

// writtenBy names who may write a field: the operator, and its writers.
func writtenBy(writers []string) string {
	if len(writers) == 0 {
		return "written by the operator"
	}
	return "written by the operator and the " + strings.Join(writers, ", ") + " " + plural(len(writers), "writer", "writers")
}

// normalize decodes raw into the kind's Go type and back to unstructured JSON,
// which drops the omitempty fields sent as their zero value. A kind the scheme
// does not know is compared as it was sent.
func (v *OperatorManagedValidator) normalize(gvk schema.GroupVersionKind, raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if v.Scheme != nil {
		if typed, err := v.Scheme.New(gvk); err == nil {
			if err := json.Unmarshal(raw, typed); err != nil {
				return nil, err
			}
			b, err := json.Marshal(typed)
			if err != nil {
				return nil, err
			}
			raw = b
		}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// project keeps only what lies along segments in obj. A list maps each element
// along the same segments, because the generator gives list elements no segment
// of their own; "*" maps each value of an object. Comparing two projections is
// comparing the field, wherever in the object it repeats.
func project(obj any, segments []string) any {
	if len(segments) == 0 {
		return obj
	}
	switch node := obj.(type) {
	case []any:
		out := make([]any, len(node))
		for i, element := range node {
			out[i] = project(element, segments)
		}
		return out
	case map[string]any:
		if segments[0] == "*" {
			out := make(map[string]any, len(node))
			for key, value := range node {
				out[key] = project(value, segments[1:])
			}
			return out
		}
		return project(node[segments[0]], segments[1:])
	default:
		return nil
	}
}
