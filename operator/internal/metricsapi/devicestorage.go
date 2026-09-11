// The REST storage behind metrics.simplyblock.io/v1alpha2 storagedevicemetrics,
// and the `kubectl get sdm` columns that go with it.
//
// It is the same shape as the logicalvolumemetrics storage next door and joins a
// different pair. A volume's identity comes from the claim that names it, and a
// device's comes from its own StorageDevice object: the mirror has already given
// every device a Kubernetes name and a namespace, so this storage reads those
// objects and asks Prometheus what each one holds.
//
// A device with no object is not served. The object is the identity, and a
// reading under a name nothing else in the cluster knows is a reading nobody can
// correlate. A device with no sample is not served either: zeros are the reading
// of an empty device rather than the absence of a reading.

package metricsapi

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"

	"github.com/simplyblock/atlas/prometheus"

	metricsv1alpha2 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha2"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// DeviceResourceName is the plural resource these readings are served under. It
// is its own singular as well, the way `endpoints` is: "metrics" is already the
// noun, and "storagedevicemetric" is not a word.
const DeviceResourceName = "storagedevicemetrics"

// DeviceShortName is the abbreviation `kubectl get sdm` resolves.
const DeviceShortName = "sdm"

// DeviceCapacitySource supplies a device's occupancy. It is satisfied by
// atlas-lib's prometheus.Provider, and it is an interface here so that a test
// needs no Prometheus and a deployment without one can pass nil.
//
// The control plane is not the source. Its DeviceDTO carries a capacity block
// its watch stream never updates, while the metrics the same service exports
// carry the current numbers.
type DeviceCapacitySource interface {
	// DeviceCapacity returns the sample for every device of a cluster, keyed by
	// control-plane device id. A device with no sample is absent.
	DeviceCapacity(ctx context.Context, clusterUUID string) (map[string]prometheus.Capacity, error)
}

// DeviceStorage serves storagedevicemetrics. It holds no state of its own: the
// identities come from the manager's Kubernetes cache and the readings from
// Prometheus, and it is the join of the two.
type DeviceStorage struct {
	reader   client.Reader
	capacity DeviceCapacitySource
}

// NewDeviceStorage returns the REST storage over the given Kubernetes reader.
//
// capacity may be nil, and a deployment with no Prometheus is why. Nothing is
// then served, because every field of a device's reading is a measurement. A
// volume differs: its provisioned size is known without measuring.
func NewDeviceStorage(reader client.Reader, capacity DeviceCapacitySource) *DeviceStorage {
	return &DeviceStorage{reader: reader, capacity: capacity}
}

// New implements rest.Storage.
func (s *DeviceStorage) New() runtime.Object {
	return &metricsv1alpha2.StorageDeviceMetrics{}
}

// Destroy implements rest.Storage. There is nothing to release: no client, no
// watch, and no connection is owned here.
func (s *DeviceStorage) Destroy() {}

// NamespaceScoped implements rest.Scoper. The resource is namespaced because
// that is what confines a reader to the namespaces they already have.
func (s *DeviceStorage) NamespaceScoped() bool {
	return true
}

// GetSingularName implements rest.SingularNameProvider.
func (s *DeviceStorage) GetSingularName() string {
	return DeviceResourceName
}

// ShortNames implements rest.ShortNamesProvider.
func (s *DeviceStorage) ShortNames() []string {
	return []string{DeviceShortName}
}

// NewList implements rest.Lister.
func (s *DeviceStorage) NewList() runtime.Object {
	return &metricsv1alpha2.StorageDeviceMetricsList{}
}

// Get implements rest.Getter. The name is a StorageDevice's, so the lookup is a
// read of that object and never a scan.
func (s *DeviceStorage) Get(
	ctx context.Context, name string, _ *metav1.GetOptions,
) (runtime.Object, error) {
	namespace := request.NamespaceValue(ctx)

	var device simplyblockv1alpha2.StorageDevice
	err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &device)
	switch {
	case apierrors.IsNotFound(err):
		return nil, apierrors.NewNotFound(metricsv1alpha2.Resource(DeviceResourceName), name)
	case err != nil:
		return nil, apierrors.NewInternalError(err)
	}

	lookup := newDeviceCapacityLookup(s.capacity, logf.FromContext(ctx))
	sample, ok := lookup.forDevice(ctx, device.Status.ClusterID, device.Spec.DeviceID)
	if !ok {
		// The device is real but nothing has measured it: a cold exporter, a
		// Prometheus that cannot be reached, or a device the control plane has
		// not sampled yet.
		return nil, apierrors.NewNotFound(metricsv1alpha2.Resource(DeviceResourceName), name)
	}
	return newDeviceReading(&device, sample), nil
}

// List implements rest.Lister. It walks the device objects rather than the
// samples, because the objects are what carry a name and a namespace and a
// sample under neither is not servable.
func (s *DeviceStorage) List(
	ctx context.Context, options *metainternalversion.ListOptions,
) (runtime.Object, error) {
	namespace := request.NamespaceValue(ctx) // "" for a cluster-wide list
	selector := labels.Everything()
	if options != nil && options.LabelSelector != nil {
		selector = options.LabelSelector
	}

	var devices simplyblockv1alpha2.StorageDeviceList
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := s.reader.List(ctx, &devices, opts...); err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	lookup := newDeviceCapacityLookup(s.capacity, logf.FromContext(ctx))

	out := &metricsv1alpha2.StorageDeviceMetricsList{}
	for i := range devices.Items {
		device := &devices.Items[i]
		if !selector.Matches(labels.Set(device.Labels)) {
			continue
		}
		sample, ok := lookup.forDevice(ctx, device.Status.ClusterID, device.Spec.DeviceID)
		if !ok {
			continue
		}
		reading := newDeviceReading(device, sample)
		if !matchesDeviceFieldSelector(options, reading) {
			continue
		}
		out.Items = append(out.Items, *reading)
	}
	return out, nil
}

// matchesDeviceFieldSelector applies the only two field selectors this resource
// can answer. They are supported because a client that passes one and is
// silently ignored gets a wrong answer rather than an error. Anything else
// selects nothing, which is the honest response to a field the object has no
// index for.
func matchesDeviceFieldSelector(
	options *metainternalversion.ListOptions, reading *metricsv1alpha2.StorageDeviceMetrics,
) bool {
	if options == nil || options.FieldSelector == nil || options.FieldSelector.Empty() {
		return true
	}
	for _, req := range options.FieldSelector.Requirements() {
		var actual string
		switch req.Field {
		case "metadata.name":
			actual = reading.Name
		case "metadata.namespace":
			actual = reading.Namespace
		default:
			return false
		}
		if (req.Operator == "=" || req.Operator == "==") && actual != req.Value {
			return false
		}
		if req.Operator == "!=" && actual == req.Value {
			return false
		}
	}
	return true
}

// deviceCapacityLookup fetches samples once per cluster for the duration of one
// request, because a list walks many devices of the same few clusters and each
// query is an HTTP round trip.
//
// A cluster whose query fails is recorded as having no samples and is not
// retried within the request. Prometheus being down therefore costs the readings
// and not the request.
type deviceCapacityLookup struct {
	source    DeviceCapacitySource
	log       logr.Logger
	byCluster map[string]map[string]prometheus.Capacity
}

func newDeviceCapacityLookup(source DeviceCapacitySource, log logr.Logger) *deviceCapacityLookup {
	return &deviceCapacityLookup{
		source:    source,
		log:       log,
		byCluster: map[string]map[string]prometheus.Capacity{},
	}
}

// forDevice returns the sample for one device and whether there is one. Unlike a
// volume's lookup the two cases are distinguished here, because every field of a
// device's reading is measured: with no sample there is nothing to serve, rather
// than a nominal size to serve without its measurements.
func (l *deviceCapacityLookup) forDevice(
	ctx context.Context, clusterUUID, deviceID string,
) (prometheus.Capacity, bool) {
	if l == nil || l.source == nil || clusterUUID == "" || deviceID == "" {
		return prometheus.Capacity{}, false
	}
	samples, ok := l.byCluster[clusterUUID]
	if !ok {
		var err error
		samples, err = l.source.DeviceCapacity(ctx, clusterUUID)
		if err != nil {
			l.log.V(1).Info("no device capacity samples for this request",
				"cluster", clusterUUID, "err", err.Error())
			samples = nil
		}
		l.byCluster[clusterUUID] = samples
	}
	sample, ok := samples[deviceID]
	if !ok || !sample.Sampled() {
		return prometheus.Capacity{}, false
	}
	return sample, true
}

// newDeviceReading assembles the served object from the device's own object and
// the last sample taken of it.
func newDeviceReading(
	device *simplyblockv1alpha2.StorageDevice, sample prometheus.Capacity,
) *metricsv1alpha2.StorageDeviceMetrics {
	return &metricsv1alpha2.StorageDeviceMetrics{
		TypeMeta: metav1.TypeMeta{
			APIVersion: metricsv1alpha2.GroupVersion.String(),
			Kind:       "StorageDeviceMetrics",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              device.Name,
			Namespace:         device.Namespace,
			Labels:            device.Labels,
			CreationTimestamp: device.CreationTimestamp,
		},
		Timestamp:   metav1.NewTime(sample.SampledAt),
		DeviceID:    device.Spec.DeviceID,
		StorageNode: device.Spec.NodeRef,
		Capacity: metricsv1alpha2.StorageDeviceCapacity{
			Total:              *resource.NewQuantity(sample.Total, resource.BinarySI),
			Used:               *resource.NewQuantity(sample.Used, resource.BinarySI),
			Free:               *resource.NewQuantity(sample.Free, resource.BinarySI),
			Provisioned:        *resource.NewQuantity(sample.Provisioned, resource.BinarySI),
			UtilizationPercent: sample.UtilizationPercent,
		},
	}
}

// deviceColumns are the table headers, in print order. What they answer: which
// device, on which node, how big it is, how much of it is in use, and how stale
// the reading is. Used beside Total is the pair the kind exists for, and the
// percentage sits with them rather than being left to the reader's arithmetic.
var deviceColumns = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name", Description: "The StorageDevice this reading is of"},
	{Name: "Node", Type: "string", Description: "The StorageNode the device belongs to"},
	{Name: "Total", Type: "string", Description: "The device's size"},
	{Name: "Used", Type: "string", Description: "The space the device holds"},
	{Name: "Used%", Type: "string", Description: "The control plane's utilization figure"},
	{Name: "Device", Type: "string", Priority: 1, Description: "The control plane's device id"},
	{Name: "Sampled", Type: "string", Description: "How long ago the control plane took the reading"},
}

// ConvertToTable implements rest.TableConvertor for both a single reading and a
// list of them.
func (s *DeviceStorage) ConvertToTable(
	_ context.Context, object runtime.Object, _ runtime.Object,
) (*metav1.Table, error) {
	table := &metav1.Table{ColumnDefinitions: deviceColumns}

	switch typed := object.(type) {
	case *metricsv1alpha2.StorageDeviceMetrics:
		table.Rows = append(table.Rows, deviceRow(typed))
	case *metricsv1alpha2.StorageDeviceMetricsList:
		table.ResourceVersion = typed.ResourceVersion
		for i := range typed.Items {
			table.Rows = append(table.Rows, deviceRow(&typed.Items[i]))
		}
	default:
		return nil, fmt.Errorf("metricsapi: cannot render %T as a table", object)
	}

	if m, err := meta.ListAccessor(object); err == nil {
		table.ResourceVersion = m.GetResourceVersion()
		table.Continue = m.GetContinue()
	}
	return table, nil
}

func deviceRow(reading *metricsv1alpha2.StorageDeviceMetrics) metav1.TableRow {
	return metav1.TableRow{
		Cells: []any{
			reading.Name,
			reading.StorageNode,
			reading.Capacity.Total.String(),
			reading.Capacity.Used.String(),
			fmt.Sprintf("%d%%", reading.Capacity.UtilizationPercent),
			reading.DeviceID,
			translateSampleAge(reading.Timestamp),
		},
		Object: runtime.RawExtension{Object: reading},
	}
}

// translateSampleAge renders how old the reading is. The age of the sample is
// the useful number here rather than the age of the object: a device object is
// as old as the drive, and the question a reader has is how current the figures
// beside it are.
func translateSampleAge(timestamp metav1.Time) string {
	if timestamp.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(timestamp.Time))
}

var (
	_ rest.Storage              = (*DeviceStorage)(nil)
	_ rest.Scoper               = (*DeviceStorage)(nil)
	_ rest.Getter               = (*DeviceStorage)(nil)
	_ rest.Lister               = (*DeviceStorage)(nil)
	_ rest.TableConvertor       = (*DeviceStorage)(nil)
	_ rest.ShortNamesProvider   = (*DeviceStorage)(nil)
	_ rest.SingularNameProvider = (*DeviceStorage)(nil)
)
