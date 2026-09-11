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

	"k8s.io/kube-openapi/pkg/validation/spec"

	metricsv1alpha2 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha2"
)

// Every kind the group serves, at the version it is served at. A kind registered
// under the wrong version is a 404 on the route a client was told to use, which
// nothing else in this package would catch.
func TestSchemeKnowsTheServedKinds(t *testing.T) {
	for _, object := range []runtime.Object{
		&metricsv1alpha2.LogicalVolumeMetrics{},
		&metricsv1alpha2.LogicalVolumeMetricsList{},
		&metricsv1alpha2.StorageDeviceMetrics{},
		&metricsv1alpha2.StorageDeviceMetricsList{},
	} {
		kinds, _, err := Scheme.ObjectKinds(object)
		if err != nil {
			t.Errorf("ObjectKinds(%T): %v", object, err)
			continue
		}
		found := false
		for _, kind := range kinds {
			if kind.GroupVersion() == metricsv1alpha2.GroupVersion {
				found = true
			}
		}
		if !found {
			t.Errorf("%T registered as %v, want %s", object, kinds, metricsv1alpha2.GroupVersion)
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
	want := &metricsv1alpha2.LogicalVolumeMetrics{
		ObjectMeta:       metav1.ObjectMeta{Name: "postgres-data", Namespace: "team-a"},
		Timestamp:        metav1.Unix(1756713600, 0),
		VolumeHandle:     "c:p:v",
		PersistentVolume: "pv-a",
		PoolName:         "pool-a",
		Capacity: metricsv1alpha2.LogicalVolumeCapacity{
			Provisioned:        *resource.NewQuantity(107374182400, resource.BinarySI),
			Used:               *resource.NewQuantity(38654705664, resource.BinarySI),
			UtilizationPercent: 36,
		},
	}

	codec := Codecs.LegacyCodec(metricsv1alpha2.GroupVersion)
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
	got, ok := decoded.(*metricsv1alpha2.LogicalVolumeMetrics)
	if !ok {
		t.Fatalf("decoded %T, want *LogicalVolumeMetrics", decoded)
	}
	if got.Name != want.Name || got.Capacity.Used.Value() != want.Capacity.Used.Value() {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// InstallAPIGroup is where a resource that does not implement what the installer
// demands is rejected, and where a kind whose OpenAPI definition is missing shows
// up. Running it without a listener proves both registrations are well-formed.
func TestAPIGroupInstalls(t *testing.T) {
	config := genericapiserver.NewRecommendedConfig(Codecs)
	config.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString(operatorAPIVersion, "", "")
	config.ExternalAddress = "metrics.simplyblock.test:443"
	// The real path gets this from the secure-serving options; the test has no
	// listener, and New refuses to build without one.
	config.LoopbackClientConfig = &restclient.Config{Host: "https://" + config.ExternalAddress}
	namer := openapinamer.NewDefinitionNamer(Scheme)
	config.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(openAPIDefinitions, namer)
	config.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(openAPIDefinitions, namer)

	server, err := config.Complete().New("test-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		t.Fatalf("build a bare api server: %v", err)
	}

	group := genericapiserver.NewDefaultAPIGroupInfo(metricsv1alpha2.GroupName, Scheme, ParameterCodec, Codecs)
	group.VersionedResourcesStorageMap[metricsv1alpha2.GroupVersion.Version] = map[string]rest.Storage{
		ResourceName:       NewStorage(fakeVolumes{}, nil, nil),
		DeviceResourceName: NewDeviceStorage(nil, nil),
	}
	if err := server.InstallAPIGroup(&group); err != nil {
		t.Fatalf("InstallAPIGroup: %v", err)
	}
}

// One served version, and it is v1alpha2. The count is the assertion: a second
// version would make the internal version a real conversion hub and would need an
// APIService object of its own, so it is a change to notice rather than to
// discover from a 404.
func TestTheGroupServesOneVersion(t *testing.T) {
	versions := Scheme.PrioritizedVersionsForGroup(metricsv1alpha2.GroupName)
	if len(versions) != 1 {
		t.Fatalf("prioritized versions = %v, want v1alpha2 alone", versions)
	}
	if versions[0] != metricsv1alpha2.GroupVersion {
		t.Errorf("served version = %s, want %s", versions[0], metricsv1alpha2.GroupVersion)
	}
}

func TestCodecRoundTripsADeviceReading(t *testing.T) {
	want := &metricsv1alpha2.StorageDeviceMetrics{
		ObjectMeta:  metav1.ObjectMeta{Name: "production-7f3a9c-5e0000a1", Namespace: "sb"},
		Timestamp:   metav1.Unix(1756713600, 0),
		DeviceID:    "5e0000a1-3b2c-4d5e-9f01-2a3b4c5d6e7f",
		StorageNode: "production-7f3a9c",
		Capacity: metricsv1alpha2.StorageDeviceCapacity{
			Total:              *resource.NewQuantity(3840755982336, resource.BinarySI),
			Used:               *resource.NewQuantity(1920377991168, resource.BinarySI),
			UtilizationPercent: 50,
		},
	}

	codec := Codecs.LegacyCodec(metricsv1alpha2.GroupVersion)
	encoded, err := runtime.Encode(codec, want)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(encoded), "\"kind\":\"StorageDeviceMetrics\"") {
		t.Errorf("encoded object carries no kind: %s", encoded)
	}

	decoded, err := runtime.Decode(codec, encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := decoded.(*metricsv1alpha2.StorageDeviceMetrics)
	if !ok {
		t.Fatalf("decoded %T, want *StorageDeviceMetrics", decoded)
	}
	if got.Name != want.Name || got.Capacity.Used.Value() != want.Capacity.Used.Value() {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// The definitions have to carry every served kind. A kind with no definition
// makes InstallAPIGroup fail on the field-management type converter, and leaves
// `kubectl explain` empty.
func TestOpenAPIDefinitionsCoverEveryServedKind(t *testing.T) {
	definitions := openAPIDefinitions(func(string) spec.Ref { return spec.Ref{} })
	const pkg = "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha2."
	for _, kind := range []string{"LogicalVolumeMetrics", "StorageDeviceMetrics"} {
		if _, ok := definitions[pkg+kind]; !ok {
			t.Errorf("no OpenAPI definition for %s", pkg+kind)
		}
	}
}

// Every served version needs its own APIService object, and the CA bundle has to
// be injected into each: the kube-apiserver trusts the listener per APIService,
// so a version whose object carries no bundle is Available=False and answers
// nothing. Deriving the list from the scheme is what stops a version being added
// to one and forgotten in the other.
func TestEveryServedVersionGetsItsCABundle(t *testing.T) {
	served := Scheme.PrioritizedVersionsForGroup(metricsv1alpha2.GroupName)
	injected := metricsAPIServiceObjects()
	if len(injected) != len(served) {
		t.Fatalf("%d APIService objects for %d served versions", len(injected), len(served))
	}
	for _, version := range served {
		want := version.Version + "." + version.Group
		found := false
		for _, name := range injected {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no CA bundle injection for %s, want the APIService %q", version, want)
		}
	}
}
