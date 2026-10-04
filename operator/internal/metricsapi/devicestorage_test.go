// Tests for the StorageDeviceMetrics REST storage: which devices are served,
// where the numbers come from, and what a namespaced client is confined to.
//
// The identity is the part worth testing. A reading is named after a
// StorageDevice object, so a device with no object must not be served under a
// name it does not have, and a namespaced list must not answer with another
// namespace's devices.

package metricsapi

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/prometheus"
	"github.com/simplyblock/atlas/ptr"

	metricsv1alpha2 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha2"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testDeviceCluster = "11111111-1111-1111-1111-111111111111"
	testDeviceNode    = "44444444-4444-4444-4444-444444444444"
	testDeviceID      = "5e0000a1-3b2c-4d5e-9f01-2a3b4c5d6e7f"
	testDeviceName    = "production-7f3a9c-5e0000a1"
	testDeviceNodeCR  = "production-7f3a9c"
)

// fakeDeviceCapacity stands in for the metrics endpoint, counting its calls
// because one list must query a cluster once rather than once per device.
type fakeDeviceCapacity struct {
	byCluster map[string]map[string]prometheus.Capacity
	err       error
	calls     int
}

func (f *fakeDeviceCapacity) DeviceCapacity(
	_ context.Context, clusterUUID string,
) (map[string]prometheus.Capacity, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byCluster[clusterUUID], nil
}

func deviceSample() prometheus.Capacity {
	return prometheus.Capacity{
		Total: 3840755982336, Used: 1920377991168, Free: 1920377991168,
		Provisioned: 3840755982336, UtilizationPercent: 50,
		SampledAt: time.Unix(1757315642, 0),
	}
}

// storageDevice is a mirror object as the device controller would have left it.
func storageDevice(namespace, name, deviceID string) *simplyblockv1alpha2.StorageDevice {
	return &simplyblockv1alpha2.StorageDevice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name,
			CreationTimestamp: metav1.Unix(1757300000, 0),
		},
		Spec: simplyblockv1alpha2.StorageDeviceSpec{NodeRef: testDeviceNodeCR, DeviceID: deviceID},
		Status: simplyblockv1alpha2.StorageDeviceStatus{
			Phase:     simplyblockv1alpha2.StorageDevicePhaseOnline,
			Capacity:  &simplyblockv1alpha2.DeviceCapacity{TotalBytes: ptr.To(int64(3840755982336))},
			ClusterID: testDeviceCluster,
			NodeID:    testDeviceNode,
		},
	}
}

func newDeviceStorage(
	t *testing.T, capacity DeviceCapacitySource, objs ...client.Object,
) *DeviceStorage {
	t.Helper()
	reader := fake.NewClientBuilder().WithScheme(newDeviceScheme(t)).WithObjects(objs...).Build()
	return NewDeviceStorage(reader, capacity)
}

func newDeviceScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := simplyblockv1alpha2.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func deviceContext(namespace string) context.Context {
	return request.WithNamespace(context.Background(), namespace)
}

func TestDeviceGetJoinsTheObjectWithItsSample(t *testing.T) {
	capacity := &fakeDeviceCapacity{byCluster: map[string]map[string]prometheus.Capacity{
		testDeviceCluster: {testDeviceID: deviceSample()},
	}}
	storage := newDeviceStorage(t, capacity, storageDevice("sb", testDeviceName, testDeviceID))

	object, err := storage.Get(deviceContext("sb"), testDeviceName, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reading, ok := object.(*metricsv1alpha2.StorageDeviceMetrics)
	if !ok {
		t.Fatalf("Get returned %T", object)
	}

	if reading.Name != testDeviceName || reading.Namespace != "sb" {
		t.Errorf("reading is named %s/%s", reading.Namespace, reading.Name)
	}
	if reading.DeviceID != testDeviceID {
		t.Errorf("deviceID = %q", reading.DeviceID)
	}
	if reading.StorageNode != testDeviceNodeCR {
		t.Errorf("storageNode = %q", reading.StorageNode)
	}
	if got := reading.Capacity.Used.Value(); got != 1920377991168 {
		t.Errorf("used = %d", got)
	}
	if got := reading.Capacity.UtilizationPercent; got != 50 {
		t.Errorf("utilization = %d", got)
	}
	// The sample time is the control plane's, not the moment of the request.
	if !reading.Timestamp.Time.Equal(deviceSample().SampledAt) {
		t.Errorf("timestamp = %v, want the sample's own", reading.Timestamp)
	}
}

// A device object with no sample is not a reading. Serving zeros would be
// indistinguishable from an empty device.
func TestDeviceGetIsNotFoundWithoutASample(t *testing.T) {
	storage := newDeviceStorage(t, &fakeDeviceCapacity{}, storageDevice("sb", testDeviceName, testDeviceID))

	if _, err := storage.Get(deviceContext("sb"), testDeviceName, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound for an unsampled device, got %v", err)
	}
}

// Without a Prometheus there is nothing to serve, and saying so is the honest
// answer: the alternative reports every device as empty.
func TestDeviceReadingsAreAbsentWithoutAProvider(t *testing.T) {
	storage := newDeviceStorage(t, nil, storageDevice("sb", testDeviceName, testDeviceID))

	if _, err := storage.Get(deviceContext("sb"), testDeviceName, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound with no capacity source, got %v", err)
	}
	object, err := storage.List(deviceContext("sb"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if list := object.(*metricsv1alpha2.StorageDeviceMetricsList); len(list.Items) != 0 {
		t.Errorf("%d readings with no capacity source", len(list.Items))
	}
}

// A name that is not a device's is not served, whatever Prometheus holds under
// it.
func TestDeviceGetIsNotFoundWithoutAnObject(t *testing.T) {
	capacity := &fakeDeviceCapacity{byCluster: map[string]map[string]prometheus.Capacity{
		testDeviceCluster: {testDeviceID: deviceSample()},
	}}
	storage := newDeviceStorage(t, capacity)

	if _, err := storage.Get(deviceContext("sb"), testDeviceName, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound without a device object, got %v", err)
	}
}

func TestDeviceListIsConfinedToItsNamespace(t *testing.T) {
	capacity := &fakeDeviceCapacity{byCluster: map[string]map[string]prometheus.Capacity{
		testDeviceCluster: {
			testDeviceID: deviceSample(),
			"other":      deviceSample(),
		},
	}}
	storage := newDeviceStorage(t, capacity,
		storageDevice("team-a", testDeviceName, testDeviceID),
		storageDevice("team-b", "other-7f3a9c-other", "other"),
	)

	object, err := storage.List(deviceContext("team-a"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	list := object.(*metricsv1alpha2.StorageDeviceMetricsList)
	if len(list.Items) != 1 {
		t.Fatalf("%d readings, want the one in team-a", len(list.Items))
	}
	if list.Items[0].Namespace != "team-a" {
		t.Errorf("served %s/%s to a team-a client", list.Items[0].Namespace, list.Items[0].Name)
	}
}

// A cluster-wide list is what a cluster administrator asks for, and it answers
// with every namespace's devices.
func TestDeviceListAcrossNamespaces(t *testing.T) {
	capacity := &fakeDeviceCapacity{byCluster: map[string]map[string]prometheus.Capacity{
		testDeviceCluster: {testDeviceID: deviceSample(), "other": deviceSample()},
	}}
	storage := newDeviceStorage(t, capacity,
		storageDevice("team-a", testDeviceName, testDeviceID),
		storageDevice("team-b", "other-7f3a9c-other", "other"),
	)

	object, err := storage.List(context.Background(), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if list := object.(*metricsv1alpha2.StorageDeviceMetricsList); len(list.Items) != 2 {
		t.Fatalf("%d readings, want 2", len(list.Items))
	}
}

// One query per cluster and not one per device: a hundred nodes with eight
// devices each is one request.
func TestDeviceListQueriesEachClusterOnce(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	samples := map[string]prometheus.Capacity{}
	devices := make([]client.Object, 0, len(ids))
	for _, id := range ids {
		samples[id] = deviceSample()
		devices = append(devices, storageDevice("sb", "production-7f3a9c-"+id, id))
	}
	capacity := &fakeDeviceCapacity{
		byCluster: map[string]map[string]prometheus.Capacity{testDeviceCluster: samples},
	}
	storage := newDeviceStorage(t, capacity, devices...)

	if _, err := storage.List(deviceContext("sb"), &metainternalversion.ListOptions{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if capacity.calls != 1 {
		t.Errorf("%d capacity queries for four devices of one cluster, want 1", capacity.calls)
	}
}

// A Prometheus that answers with an error costs the readings and not the
// request: a client asking which devices it has is told nothing rather than
// given an error to interpret.
func TestDeviceListSurvivesABrokenProvider(t *testing.T) {
	storage := newDeviceStorage(t, &fakeDeviceCapacity{err: errors.New("connection refused")},
		storageDevice("sb", testDeviceName, testDeviceID))

	object, err := storage.List(deviceContext("sb"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("a failed capacity query must not fail the list: %v", err)
	}
	if list := object.(*metricsv1alpha2.StorageDeviceMetricsList); len(list.Items) != 0 {
		t.Errorf("%d readings from a broken provider", len(list.Items))
	}
}

// The columns are part of the API: the server decides what `kubectl get sdm`
// prints, because nothing about an aggregated resource is compiled into
// kubectl.
func TestDeviceTableRendersTheReading(t *testing.T) {
	capacity := &fakeDeviceCapacity{byCluster: map[string]map[string]prometheus.Capacity{
		testDeviceCluster: {testDeviceID: deviceSample()},
	}}
	storage := newDeviceStorage(t, capacity, storageDevice("sb", testDeviceName, testDeviceID))

	object, err := storage.Get(deviceContext("sb"), testDeviceName, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	table, err := storage.ConvertToTable(context.Background(), object, nil)
	if err != nil {
		t.Fatalf("ConvertToTable: %v", err)
	}
	if len(table.Rows) != 1 {
		t.Fatalf("%d rows, want 1", len(table.Rows))
	}
	if len(table.Rows[0].Cells) != len(table.ColumnDefinitions) {
		t.Errorf("%d cells for %d columns", len(table.Rows[0].Cells), len(table.ColumnDefinitions))
	}
	if table.Rows[0].Cells[0] != testDeviceName {
		t.Errorf("first cell = %v, want the device's name", table.Rows[0].Cells[0])
	}
}

func TestDeviceStorageIdentity(t *testing.T) {
	storage := newDeviceStorage(t, nil)
	if !storage.NamespaceScoped() {
		t.Error("the resource has to be namespaced for RBAC to confine a reader")
	}
	if got := storage.ShortNames(); len(got) == 0 || got[0] != DeviceShortName {
		t.Errorf("short names = %v", got)
	}
	if _, ok := storage.New().(*metricsv1alpha2.StorageDeviceMetrics); !ok {
		t.Errorf("New returned %T", storage.New())
	}
	if _, ok := storage.NewList().(*metricsv1alpha2.StorageDeviceMetricsList); !ok {
		t.Errorf("NewList returned %T", storage.NewList())
	}
}
