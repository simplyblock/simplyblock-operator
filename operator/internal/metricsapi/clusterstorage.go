// The REST storage behind metrics.simplyblock.io/v1alpha2 storageclustermetrics,
// and the `kubectl get scm` columns that go with it.
//
// It is the same shape as the three storages next door and joins the same kind
// of pair: the identity comes from the StorageCluster object, which already has
// a name and a namespace, and the reading comes from the control plane's
// exporter.
//
// A cluster with no object is not served. The object is the identity, and a
// reading under a name nothing else in the cluster knows is a reading nobody
// can correlate. A cluster with no UUID is not served either, because it has
// not been created in the control plane and there is nothing to have measured;
// nor is one with no sample, because zeros are the reading of an empty cluster
// rather than the absence of a reading.
//
// One thing differs from the three next door, and it is the exporter's own
// shape rather than a choice here: a cluster's series are keyed by the same
// label every query already filters on, so one query per cluster returns one
// sample instead of a map.

package metricsapi

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/prometheus"

	metricsv1alpha2 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha2"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// ClusterResourceName is the plural resource these readings are served under.
// It is its own singular as well, the way `endpoints` is: "metrics" is already
// the noun, and "storageclustermetric" is not a word.
const ClusterResourceName = "storageclustermetrics"

// ClusterShortName is the abbreviation `kubectl get scm` resolves.
const ClusterShortName = "scm"

// ClusterCapacitySource supplies a cluster's occupancy. It is satisfied by
// atlas-lib's prometheus.Provider, and it is an interface here so that a test
// needs no Prometheus and a deployment without one can pass nil.
type ClusterCapacitySource interface {
	// ClusterCapacity returns the sample for one cluster, and whether there is
	// one. A cluster with no sample yields false rather than zeros.
	ClusterCapacity(ctx context.Context, clusterUUID string) (prometheus.Capacity, bool, error)
}

// ClusterStorage serves storageclustermetrics. It holds no state of its own:
// the identities come from the manager's Kubernetes cache and the readings from
// Prometheus, and it is the join of the two.
type ClusterStorage struct {
	reader   client.Reader
	capacity ClusterCapacitySource
}

// NewClusterStorage returns the REST storage over the given Kubernetes reader.
//
// capacity may be nil, and a deployment with no Prometheus is why. Nothing is
// then served, because every field of a cluster's reading is a measurement: the
// cluster's own layout is in its spec and is not a reading of anything.
func NewClusterStorage(reader client.Reader, capacity ClusterCapacitySource) *ClusterStorage {
	return &ClusterStorage{reader: reader, capacity: capacity}
}

// New implements rest.Storage.
func (s *ClusterStorage) New() runtime.Object { return &metricsv1alpha2.StorageClusterMetrics{} }

// Destroy implements rest.Storage. There is nothing to release: no client, no
// watch, and no connection is owned here.
func (s *ClusterStorage) Destroy() {}

// NamespaceScoped implements rest.Scoper. The resource is namespaced because
// that is what confines a reader to the namespaces they already have.
func (s *ClusterStorage) NamespaceScoped() bool { return true }

// GetSingularName implements rest.SingularNameProvider.
func (s *ClusterStorage) GetSingularName() string { return ClusterResourceName }

// ShortNames implements rest.ShortNamesProvider.
func (s *ClusterStorage) ShortNames() []string { return []string{ClusterShortName} }

// NewList implements rest.Lister.
func (s *ClusterStorage) NewList() runtime.Object {
	return &metricsv1alpha2.StorageClusterMetricsList{}
}

// Get implements rest.Getter. The name is a StorageCluster's, so the lookup is
// a read of that object and never a scan.
func (s *ClusterStorage) Get(
	ctx context.Context, name string, _ *metav1.GetOptions,
) (runtime.Object, error) {
	namespace := request.NamespaceValue(ctx)

	var cluster simplyblockv1alpha2.StorageCluster
	err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cluster)
	switch {
	case apierrors.IsNotFound(err):
		return nil, apierrors.NewNotFound(metricsv1alpha2.Resource(ClusterResourceName), name)
	case err != nil:
		return nil, apierrors.NewInternalError(err)
	}

	sample, ok := s.sampleFor(ctx, &cluster)
	if !ok {
		// The cluster is real but nothing has measured it: a cold exporter, a
		// Prometheus that cannot be reached, or a cluster the control plane
		// does not have yet.
		return nil, apierrors.NewNotFound(metricsv1alpha2.Resource(ClusterResourceName), name)
	}
	return newClusterReading(&cluster, sample), nil
}

// List implements rest.Lister. It walks the cluster objects rather than the
// samples, because the objects are what carry a name and a namespace and a
// sample under neither is not servable.
func (s *ClusterStorage) List(
	ctx context.Context, options *metainternalversion.ListOptions,
) (runtime.Object, error) {
	namespace := request.NamespaceValue(ctx) // "" for a cluster-wide list
	selector := labels.Everything()
	if options != nil && options.LabelSelector != nil {
		selector = options.LabelSelector
	}

	var clusters simplyblockv1alpha2.StorageClusterList
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := s.reader.List(ctx, &clusters, opts...); err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	out := &metricsv1alpha2.StorageClusterMetricsList{}
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		if !selector.Matches(labels.Set(cluster.Labels)) {
			continue
		}
		sample, ok := s.sampleFor(ctx, cluster)
		if !ok {
			continue
		}
		reading := newClusterReading(cluster, sample)
		if !matchesClusterFieldSelector(options, reading) {
			continue
		}
		out.Items = append(out.Items, *reading)
	}
	return out, nil
}

// sampleFor reads one cluster's occupancy, and reports false when there is no
// reading to serve.
//
// A query failure costs the reading and not the request, which is the same
// trade the three storages next door make: a Prometheus that is down leaves the
// list short rather than failing it.
func (s *ClusterStorage) sampleFor(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (prometheus.Capacity, bool) {
	if s.capacity == nil || cluster.Status.UUID == "" {
		return prometheus.Capacity{}, false
	}
	sample, ok, err := s.capacity.ClusterCapacity(ctx, cluster.Status.UUID)
	if err != nil {
		logf.FromContext(ctx).V(1).Info("no cluster capacity sample for this request",
			"cluster", cluster.Status.UUID, "err", err.Error())
		return prometheus.Capacity{}, false
	}
	return sample, ok
}

// matchesClusterFieldSelector applies the only two field selectors this
// resource can answer. They are supported because a client that passes one and
// is silently ignored gets a wrong answer rather than an error. Anything else
// selects nothing, which is the honest response to a field the object has no
// index for.
func matchesClusterFieldSelector(
	options *metainternalversion.ListOptions, reading *metricsv1alpha2.StorageClusterMetrics,
) bool {
	if options == nil || options.FieldSelector == nil || options.FieldSelector.Empty() {
		return true
	}
	for _, req := range options.FieldSelector.Requirements() {
		var actual string
		switch req.Field {
		case fieldSelectorName:
			actual = reading.Name
		case fieldSelectorNamespace:
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

// newClusterReading assembles the served object from the cluster's own object
// and the last sample taken of it.
func newClusterReading(
	cluster *simplyblockv1alpha2.StorageCluster, sample prometheus.Capacity,
) *metricsv1alpha2.StorageClusterMetrics {
	return &metricsv1alpha2.StorageClusterMetrics{
		TypeMeta: metav1.TypeMeta{
			APIVersion: metricsv1alpha2.GroupVersion.String(),
			Kind:       "StorageClusterMetrics",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              cluster.Name,
			Namespace:         cluster.Namespace,
			Labels:            cluster.Labels,
			CreationTimestamp: cluster.CreationTimestamp,
		},
		Timestamp:           metav1.NewTime(sample.SampledAt),
		ClusterID:           cluster.Status.UUID,
		ErasureCodingScheme: cluster.Status.ErasureCodingScheme,
		Capacity: metricsv1alpha2.StorageClusterCapacity{
			Total:              *resource.NewQuantity(sample.Total, resource.BinarySI),
			Used:               *resource.NewQuantity(sample.Used, resource.BinarySI),
			Free:               *resource.NewQuantity(sample.Free, resource.BinarySI),
			Provisioned:        *resource.NewQuantity(sample.Provisioned, resource.BinarySI),
			UtilizationPercent: sample.UtilizationPercent,
		},
	}
}

// clusterColumns are the table headers, in print order. What they answer: which
// cluster, how big it is, how much of it is in use, and how much has been
// promised out of it. The erasure-coding scheme sits beside the total because
// the two are read together — a raw total means something different at 2x1 than
// at 4x2 — and it is the cheapest way to keep a capacity plan honest.
var clusterColumns = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name", Description: "The StorageCluster this reading is of"},
	{Name: "Total", Type: "string", Description: "The capacity the cluster's devices add up to"},
	{Name: "Used", Type: "string", Description: "The space the cluster's volumes occupy"},
	{Name: "Provisioned", Type: "string", Description: "The space the cluster's volumes were promised"},
	{Name: "Used%", Type: "string", Description: "The control plane's utilization figure"},
	{Name: "EC", Type: "string", Description: "The active erasure-coding layout"},
	{Name: "Cluster", Type: "string", Priority: 1, Description: "The control plane's cluster id"},
}

// ConvertToTable implements rest.TableConvertor for both a single reading and a
// list of them.
func (s *ClusterStorage) ConvertToTable(
	_ context.Context, object runtime.Object, _ runtime.Object,
) (*metav1.Table, error) {
	table := &metav1.Table{ColumnDefinitions: clusterColumns}

	switch typed := object.(type) {
	case *metricsv1alpha2.StorageClusterMetrics:
		table.Rows = append(table.Rows, clusterRow(typed))
	case *metricsv1alpha2.StorageClusterMetricsList:
		table.ResourceVersion = typed.ResourceVersion
		for i := range typed.Items {
			table.Rows = append(table.Rows, clusterRow(&typed.Items[i]))
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

func clusterRow(reading *metricsv1alpha2.StorageClusterMetrics) metav1.TableRow {
	return metav1.TableRow{
		Cells: []any{
			reading.Name,
			reading.Capacity.Total.String(),
			reading.Capacity.Used.String(),
			reading.Capacity.Provisioned.String(),
			fmt.Sprintf("%d%%", reading.Capacity.UtilizationPercent),
			reading.ErasureCodingScheme,
			reading.ClusterID,
		},
		Object: runtime.RawExtension{Object: reading},
	}
}

var (
	_ rest.Storage              = (*ClusterStorage)(nil)
	_ rest.Scoper               = (*ClusterStorage)(nil)
	_ rest.Getter               = (*ClusterStorage)(nil)
	_ rest.Lister               = (*ClusterStorage)(nil)
	_ rest.TableConvertor       = (*ClusterStorage)(nil)
	_ rest.ShortNamesProvider   = (*ClusterStorage)(nil)
	_ rest.SingularNameProvider = (*ClusterStorage)(nil)
)
