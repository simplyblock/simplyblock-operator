// The REST storage behind metrics.simplyblock.io/v1alpha2 storagepoolmetrics,
// and the `kubectl get spm` columns that go with it.
//
// It is the same shape as the two storages next door and joins the same kind of
// pair: the identity comes from the StoragePool object, which already has a name
// and a namespace, and the reading comes from the control plane's exporter.
//
// A pool with no object is not served. The object is the identity, and a reading
// under a name nothing else in the cluster knows is a reading nobody can
// correlate. A pool with no sample is not served either: zeros are the reading
// of an empty pool rather than the absence of a reading.
//
// One thing differs from the device storage, and it is the control plane's
// asymmetry rather than a choice here. There is no pool_date series, so a pool's
// sample carries no timestamp and "has a reading" cannot be asked of the sample
// itself; presence in the query's result is what answers it.

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

	"github.com/go-logr/logr"

	"github.com/simplyblock/atlas/prometheus"

	metricsv1alpha2 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha2"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// PoolResourceName is the plural resource these readings are served under. It is
// its own singular as well, the way `endpoints` is: "metrics" is already the
// noun, and "storagepoolmetric" is not a word.
const PoolResourceName = "storagepoolmetrics"

// PoolShortName is the abbreviation `kubectl get spm` resolves.
const PoolShortName = "spm"

// PoolCapacitySource supplies a pool's occupancy. It is satisfied by atlas-lib's
// prometheus.Provider, and it is an interface here so that a test needs no
// Prometheus and a deployment without one can pass nil.
type PoolCapacitySource interface {
	// PoolCapacity returns the sample for every pool of a cluster, keyed by
	// control-plane pool id. A pool with no sample is absent.
	PoolCapacity(ctx context.Context, clusterUUID string) (map[string]prometheus.Capacity, error)
}

// PoolStorage serves storagepoolmetrics. It holds no state of its own: the
// identities come from the manager's Kubernetes cache and the readings from
// Prometheus, and it is the join of the two.
type PoolStorage struct {
	reader   client.Reader
	capacity PoolCapacitySource
}

// NewPoolStorage returns the REST storage over the given Kubernetes reader.
//
// capacity may be nil, and a deployment with no Prometheus is why. Nothing is
// then served, because every field of a pool's reading is a measurement: the
// pool's own limit is in its spec and is not a reading of anything.
func NewPoolStorage(reader client.Reader, capacity PoolCapacitySource) *PoolStorage {
	return &PoolStorage{reader: reader, capacity: capacity}
}

// New implements rest.Storage.
func (s *PoolStorage) New() runtime.Object { return &metricsv1alpha2.StoragePoolMetrics{} }

// Destroy implements rest.Storage. There is nothing to release: no client, no
// watch, and no connection is owned here.
func (s *PoolStorage) Destroy() {}

// NamespaceScoped implements rest.Scoper. The resource is namespaced because
// that is what confines a reader to the namespaces they already have.
func (s *PoolStorage) NamespaceScoped() bool { return true }

// GetSingularName implements rest.SingularNameProvider.
func (s *PoolStorage) GetSingularName() string { return PoolResourceName }

// ShortNames implements rest.ShortNamesProvider.
func (s *PoolStorage) ShortNames() []string { return []string{PoolShortName} }

// NewList implements rest.Lister.
func (s *PoolStorage) NewList() runtime.Object {
	return &metricsv1alpha2.StoragePoolMetricsList{}
}

// Get implements rest.Getter. The name is a StoragePool's, so the lookup is a
// read of that object and never a scan.
func (s *PoolStorage) Get(
	ctx context.Context, name string, _ *metav1.GetOptions,
) (runtime.Object, error) {
	namespace := request.NamespaceValue(ctx)

	var p simplyblockv1alpha2.StoragePool
	err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &p)
	switch {
	case apierrors.IsNotFound(err):
		return nil, apierrors.NewNotFound(metricsv1alpha2.Resource(PoolResourceName), name)
	case err != nil:
		return nil, apierrors.NewInternalError(err)
	}

	lookup := newPoolCapacityLookup(s.reader, s.capacity, logf.FromContext(ctx))
	sample, clusterUUID, ok := lookup.forPool(ctx, &p)
	if !ok {
		// The pool is real but nothing has measured it: a cold exporter, a
		// Prometheus that cannot be reached, or a pool the control plane does
		// not have yet.
		return nil, apierrors.NewNotFound(metricsv1alpha2.Resource(PoolResourceName), name)
	}
	return newPoolReading(&p, clusterUUID, sample), nil
}

// List implements rest.Lister. It walks the pool objects rather than the
// samples, because the objects are what carry a name and a namespace and a
// sample under neither is not servable.
func (s *PoolStorage) List(
	ctx context.Context, options *metainternalversion.ListOptions,
) (runtime.Object, error) {
	namespace := request.NamespaceValue(ctx) // "" for a cluster-wide list
	selector := labels.Everything()
	if options != nil && options.LabelSelector != nil {
		selector = options.LabelSelector
	}

	var pools simplyblockv1alpha2.StoragePoolList
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := s.reader.List(ctx, &pools, opts...); err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	lookup := newPoolCapacityLookup(s.reader, s.capacity, logf.FromContext(ctx))

	out := &metricsv1alpha2.StoragePoolMetricsList{}
	for i := range pools.Items {
		p := &pools.Items[i]
		if !selector.Matches(labels.Set(p.Labels)) {
			continue
		}
		sample, clusterUUID, ok := lookup.forPool(ctx, p)
		if !ok {
			continue
		}
		reading := newPoolReading(p, clusterUUID, sample)
		if !matchesPoolFieldSelector(options, reading) {
			continue
		}
		out.Items = append(out.Items, *reading)
	}
	return out, nil
}

// matchesPoolFieldSelector applies the only two field selectors this resource
// can answer. They are supported because a client that passes one and is
// silently ignored gets a wrong answer rather than an error. Anything else
// selects nothing, which is the honest response to a field the object has no
// index for.
func matchesPoolFieldSelector(
	options *metainternalversion.ListOptions, reading *metricsv1alpha2.StoragePoolMetrics,
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

// poolCapacityLookup resolves a pool's cluster and fetches that cluster's
// samples once for the duration of one request, because a list walks many pools
// of the same few clusters and each query is an HTTP round trip.
//
// A cluster whose query fails is recorded as having no samples and is not
// retried within the request. Prometheus being down therefore costs the readings
// and not the request.
type poolCapacityLookup struct {
	reader      client.Reader
	source      PoolCapacitySource
	log         logr.Logger
	byCluster   map[string]map[string]prometheus.Capacity
	clusterUUID map[string]string
}

func newPoolCapacityLookup(
	reader client.Reader, source PoolCapacitySource, log logr.Logger,
) *poolCapacityLookup {
	return &poolCapacityLookup{
		reader:      reader,
		source:      source,
		log:         log,
		byCluster:   map[string]map[string]prometheus.Capacity{},
		clusterUUID: map[string]string{},
	}
}

// forPool returns the sample for one pool, the cluster UUID it was found under,
// and whether there is one.
//
// Presence in the map is the test rather than Capacity.Sampled, because the
// control plane exports no pool_date and asking whether a pool sample was
// sampled would reject every one of them.
func (l *poolCapacityLookup) forPool(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool,
) (prometheus.Capacity, string, bool) {
	if l == nil || l.source == nil || p.Status.UUID == "" {
		return prometheus.Capacity{}, "", false
	}
	clusterUUID := l.resolveCluster(ctx, p)
	if clusterUUID == "" {
		return prometheus.Capacity{}, "", false
	}
	samples, ok := l.byCluster[clusterUUID]
	if !ok {
		var err error
		samples, err = l.source.PoolCapacity(ctx, clusterUUID)
		if err != nil {
			l.log.V(1).Info("no pool capacity samples for this request",
				"cluster", clusterUUID, "err", err.Error())
			samples = nil
		}
		l.byCluster[clusterUUID] = samples
	}
	sample, ok := samples[p.Status.UUID]
	return sample, clusterUUID, ok
}

// resolveCluster reads the UUID the exporter labels a pool's series with, which
// the pool object does not carry itself.
func (l *poolCapacityLookup) resolveCluster(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool,
) string {
	key := p.Namespace + "/" + p.Spec.ClusterRef
	if uuid, ok := l.clusterUUID[key]; ok {
		return uuid
	}
	var cluster simplyblockv1alpha1.StorageCluster
	objectKey := client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.ClusterRef}
	if err := l.reader.Get(ctx, objectKey, &cluster); err != nil {
		l.clusterUUID[key] = ""
		return ""
	}
	l.clusterUUID[key] = cluster.Status.UUID
	return cluster.Status.UUID
}

// newPoolReading assembles the served object from the pool's own object and the
// last sample taken of it.
func newPoolReading(
	p *simplyblockv1alpha2.StoragePool, clusterUUID string, sample prometheus.Capacity,
) *metricsv1alpha2.StoragePoolMetrics {
	return &metricsv1alpha2.StoragePoolMetrics{
		TypeMeta: metav1.TypeMeta{
			APIVersion: metricsv1alpha2.GroupVersion.String(),
			Kind:       "StoragePoolMetrics",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              p.Name,
			Namespace:         p.Namespace,
			Labels:            p.Labels,
			CreationTimestamp: p.CreationTimestamp,
		},
		Timestamp: metav1.NewTime(sample.SampledAt),
		PoolID:    p.Status.UUID,
		ClusterID: clusterUUID,
		Capacity: metricsv1alpha2.StoragePoolCapacity{
			Total:              *resource.NewQuantity(sample.Total, resource.BinarySI),
			Used:               *resource.NewQuantity(sample.Used, resource.BinarySI),
			Free:               *resource.NewQuantity(sample.Free, resource.BinarySI),
			Provisioned:        *resource.NewQuantity(sample.Provisioned, resource.BinarySI),
			UtilizationPercent: sample.UtilizationPercent,
		},
	}
}

// poolColumns are the table headers, in print order. What they answer: which
// pool, how big it is, how much of it is in use, and how much has been promised
// out of it. Provisioned sits beside Used because a thin-provisioned pool can be
// nearly empty and fully committed at the same time, and only one of those two
// numbers says so.
var poolColumns = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name", Description: "The StoragePool this reading is of"},
	{Name: "Total", Type: "string", Description: "The capacity the pool is charged against"},
	{Name: "Used", Type: "string", Description: "The space the pool's volumes occupy"},
	{Name: "Provisioned", Type: "string", Description: "The space the pool's volumes were promised"},
	{Name: "Used%", Type: "string", Description: "The control plane's utilization figure"},
	{Name: "Pool", Type: "string", Priority: 1, Description: "The control plane's pool id"},
}

// ConvertToTable implements rest.TableConvertor for both a single reading and a
// list of them.
func (s *PoolStorage) ConvertToTable(
	_ context.Context, object runtime.Object, _ runtime.Object,
) (*metav1.Table, error) {
	table := &metav1.Table{ColumnDefinitions: poolColumns}

	switch typed := object.(type) {
	case *metricsv1alpha2.StoragePoolMetrics:
		table.Rows = append(table.Rows, poolRow(typed))
	case *metricsv1alpha2.StoragePoolMetricsList:
		table.ResourceVersion = typed.ResourceVersion
		for i := range typed.Items {
			table.Rows = append(table.Rows, poolRow(&typed.Items[i]))
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

func poolRow(reading *metricsv1alpha2.StoragePoolMetrics) metav1.TableRow {
	return metav1.TableRow{
		Cells: []any{
			reading.Name,
			reading.Capacity.Total.String(),
			reading.Capacity.Used.String(),
			reading.Capacity.Provisioned.String(),
			fmt.Sprintf("%d%%", reading.Capacity.UtilizationPercent),
			reading.PoolID,
		},
		Object: runtime.RawExtension{Object: reading},
	}
}

var (
	_ rest.Storage              = (*PoolStorage)(nil)
	_ rest.Scoper               = (*PoolStorage)(nil)
	_ rest.Getter               = (*PoolStorage)(nil)
	_ rest.Lister               = (*PoolStorage)(nil)
	_ rest.TableConvertor       = (*PoolStorage)(nil)
	_ rest.ShortNamesProvider   = (*PoolStorage)(nil)
	_ rest.SingularNameProvider = (*PoolStorage)(nil)
)
