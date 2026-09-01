// Tests for the scheme and the API group installation.
//
// These cover the failure mode this package is otherwise blind to. A missing
// type registration, an unencodable Status, or a resource the installer refuses
// does not fail at compile time and does not fail in the storage tests next
// door: it fails when the kube-apiserver first proxies a request, as a 500 in
// somebody's cluster. Installing the group and round-tripping an object through
// the codec here moves that discovery to the build.

package metricsapi

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	restclient "k8s.io/client-go/rest"
	basecompatibility "k8s.io/component-base/compatibility"

	metricsv1alpha1 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha1"
)

func TestSchemeKnowsTheServedKinds(t *testing.T) {
	for _, obj := range []runtime.Object{
		&metricsv1alpha1.LogicalVolumeMetrics{},
		&metricsv1alpha1.LogicalVolumeMetricsList{},
	} {
		kinds, _, err := Scheme.ObjectKinds(obj)
		if err != nil {
			t.Errorf("ObjectKinds(%T): %v", obj, err)
			continue
		}
		if len(kinds) == 0 || kinds[0].Group != metricsv1alpha1.GroupName {
			t.Errorf("%T registered as %v, want group %s", obj, kinds, metricsv1alpha1.GroupName)
		}
	}
}

// A NotFound has to be encodable or the client sees a serialization failure
// instead of the error, which is the least debuggable outcome available.
func TestSchemeCanEncodeAStatus(t *testing.T) {
	unversioned := schema.GroupVersion{Group: "", Version: "v1"}
	if _, _, err := Scheme.ObjectKinds(&metav1.Status{}); err != nil {
		t.Fatalf("metav1.Status is not registered: %v", err)
	}
	if !Scheme.Recognizes(unversioned.WithKind("Status")) {
		t.Error("metav1.Status is not recognized in the unversioned group")
	}
}

func TestCodecRoundTripsAReading(t *testing.T) {
	want := &metricsv1alpha1.LogicalVolumeMetrics{
		ObjectMeta:       metav1.ObjectMeta{Name: "postgres-data", Namespace: "team-a"},
		Timestamp:        metav1.Unix(1756713600, 0),
		VolumeHandle:     "c:p:v",
		PersistentVolume: "pv-a",
		PoolName:         "pool-a",
		Capacity: metricsv1alpha1.LogicalVolumeCapacity{
			Provisioned:        *resource.NewQuantity(107374182400, resource.BinarySI),
			Used:               *resource.NewQuantity(38654705664, resource.BinarySI),
			UtilizationPercent: 36,
		},
	}

	codec := Codecs.LegacyCodec(metricsv1alpha1.GroupVersion)
	encoded, err := runtime.Encode(codec, want)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// The apiVersion and kind must survive, or a client cannot tell what it got.
	if !strings.Contains(string(encoded), `"kind":"LogicalVolumeMetrics"`) {
		t.Errorf("encoded object carries no kind: %s", encoded)
	}

	decoded, err := runtime.Decode(codec, encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := decoded.(*metricsv1alpha1.LogicalVolumeMetrics)
	if !ok {
		t.Fatalf("decoded %T, want *LogicalVolumeMetrics", decoded)
	}
	if got.Name != want.Name || got.Capacity.Used.Value() != want.Capacity.Used.Value() {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// InstallAPIGroup is where a resource that does not implement what the installer
// demands is rejected. Running it without a listener proves the registration is
// well-formed.
func TestAPIGroupInstalls(t *testing.T) {
	config := genericapiserver.NewRecommendedConfig(Codecs)
	config.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString(operatorAPIVersion, "", "")
	config.ExternalAddress = "metrics.simplyblock.test:443"
	// The real path gets this from the secure-serving options; the test has no
	// listener, and New refuses to build without one.
	config.LoopbackClientConfig = &restclient.Config{Host: "https://" + config.ExternalAddress}
	namer := openapinamer.NewDefinitionNamer(Scheme)
	config.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(metricsv1alpha1.GetOpenAPIDefinitions, namer)
	config.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(metricsv1alpha1.GetOpenAPIDefinitions, namer)

	server, err := config.Complete().New("test-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		t.Fatalf("build a bare api server: %v", err)
	}

	group := genericapiserver.NewDefaultAPIGroupInfo(metricsv1alpha1.GroupName, Scheme, ParameterCodec, Codecs)
	group.VersionedResourcesStorageMap[metricsv1alpha1.GroupVersion.Version] = map[string]rest.Storage{
		ResourceName: NewStorage(fakeVolumes{}, nil),
	}
	if err := server.InstallAPIGroup(&group); err != nil {
		t.Fatalf("InstallAPIGroup: %v", err)
	}
}
