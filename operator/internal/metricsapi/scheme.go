// The runtime scheme the aggregated API server encodes and decodes with.
//
// It is separate from the operator's own scheme in cmd/main.go on purpose. That
// one is the controller-runtime client's, and it must contain every kind a
// reconciler touches. This one is the wire contract of one API group: adding a
// kind to it publishes that kind, and nothing else may be served. Keeping them
// apart is what stops a type registered for a controller's convenience from
// silently appearing in the aggregated API's discovery document.
//
// The kinds are registered twice: once under v1alpha1, and once under the
// group's internal version. That looks redundant with one external version, and
// it is not optional: the codec's decode side targets the internal version of
// whatever group it is decoding, and a group with no internal version registered
// fails every decode with "no kind is registered for the internal version." The
// same Go type serves both, so the conversion the codec then performs hits the
// scheme's already-in-the-target-version fast path and copies nothing.
//
// A second external version is the point at which the internal version stops
// being an alias and becomes a real hub with real conversions.

package metricsapi

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	metricsv1alpha1 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha1"
)

var (
	// Scheme knows the kinds this API group serves.
	Scheme = runtime.NewScheme()
	// Codecs negotiates the response encoding: JSON, YAML, or protobuf.
	Codecs = serializer.NewCodecFactory(Scheme)
	// ParameterCodec decodes the query string into list and get options.
	ParameterCodec = runtime.NewParameterCodec(Scheme)
)

func init() {
	utilruntime.Must(addToScheme(Scheme))
}

func addToScheme(scheme *runtime.Scheme) error {
	gv := metricsv1alpha1.GroupVersion
	internal := schema.GroupVersion{Group: metricsv1alpha1.GroupName, Version: runtime.APIVersionInternal}
	for _, version := range []schema.GroupVersion{gv, internal} {
		scheme.AddKnownTypes(version,
			&metricsv1alpha1.LogicalVolumeMetrics{},
			&metricsv1alpha1.LogicalVolumeMetricsList{},
		)
	}
	// The meta kinds are registered twice as well, and for a reason that is not
	// symmetry. Under the group's own version they back this group's list and
	// get options; under the group-less "v1" they back the ListOptions the
	// endpoint installer looks for by that exact name, because
	// APIGroupInfo.OptionsExternalVersion is v1 for every group. Registering only
	// the first fails at InstallAPIGroup with `no kind "ListOptions" is
	// registered for version "v1"`.
	metav1.AddToGroupVersion(scheme, gv)
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Group: "", Version: "v1"})

	// The meta kinds every group answers with, in the group-less version they
	// are defined in. Without Status registered, an error response cannot be
	// encoded and a NotFound reaches the client as a serialization failure.
	unversioned := schema.GroupVersion{Group: "", Version: "v1"}
	scheme.AddUnversionedTypes(unversioned,
		&metav1.Status{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	)

	return scheme.SetVersionPriority(gv)
}
