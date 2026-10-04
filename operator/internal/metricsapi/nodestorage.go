// The REST storage behind metrics.simplyblock.io/v1alpha2 storagenodemetrics,
// and the `kubectl get snm` columns that go with it.
//
// It is the same shape as the storagedevicemetrics storage next door and joins
// one level up. A device's identity comes from its own StorageDevice object and
// carries the cluster id with it; a node's comes from its StorageNode object,
// which names its cluster rather than the cluster's UUID, so the join reads the
// StorageCluster once per cluster per request to learn the id Prometheus keys on.
//
// A node with no object is not served. The object is the identity, and a reading
// under a name nothing else in the cluster knows is a reading nobody can
// correlate. A node with no sample is not served either: zeros are the reading of
// an empty node rather than the absence of a reading.

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
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// NodeResourceName is the plural resource these readings are served under. It is
// its own singular as well, the way `endpoints` is: "metrics" is already the
// noun, and "storagenodemetric" is not a word.
const NodeResourceName = "storagenodemetrics"

// NodeShortName is the abbreviation `kubectl get snm` resolves.
const NodeShortName = "snm"

// NodeCapacitySource supplies a node's occupancy. It is satisfied by atlas-lib's
// prometheus.Provider, and it is an interface here so that a test needs no
// Prometheus and a deployment without one can pass nil.
//
// The control plane is not the source. Neither its node list nor its node stream
// carries how full a node is; the numbers exist only in the metrics the same
// service exports (design-storagenode.md §12).
type NodeCapacitySource interface {
	// NodeCapacity returns the sample for every node of a cluster, keyed by
	// control-plane node UUID. A node with no sample is absent.
	NodeCapacity(ctx context.Context, clusterUUID string) (map[string]prometheus.Capacity, error)
}

// NodeStorage serves storagenodemetrics. It holds no state of its own: the
// identities come from the manager's Kubernetes cache and the readings from
// Prometheus, and it is the join of the two.
type NodeStorage struct {
	reader   client.Reader
	capacity NodeCapacitySource
}

// NewNodeStorage returns the REST storage over the given Kubernetes reader.
//
// capacity may be nil, and a deployment with no Prometheus is why. Nothing is
// then served, because every field of a node's reading is a measurement.
func NewNodeStorage(reader client.Reader, capacity NodeCapacitySource) *NodeStorage {
	return &NodeStorage{reader: reader, capacity: capacity}
}

// New implements rest.Storage.
func (s *NodeStorage) New() runtime.Object { return &metricsv1alpha2.StorageNodeMetrics{} }

// Destroy implements rest.Storage. There is nothing to release: no client, no
// watch, and no connection is owned here.
func (s *NodeStorage) Destroy() {}

// NamespaceScoped implements rest.Scoper. The resource is namespaced because that
// is what confines a reader to the namespaces they already have.
func (s *NodeStorage) NamespaceScoped() bool { return true }

// GetSingularName implements rest.SingularNameProvider.
func (s *NodeStorage) GetSingularName() string { return NodeResourceName }

// ShortNames implements rest.ShortNamesProvider.
func (s *NodeStorage) ShortNames() []string { return []string{NodeShortName} }

// NewList implements rest.Lister.
func (s *NodeStorage) NewList() runtime.Object {
	return &metricsv1alpha2.StorageNodeMetricsList{}
}

// Get implements rest.Getter. The name is a StorageNode's, so the lookup is a
// read of that object and never a scan.
func (s *NodeStorage) Get(
	ctx context.Context, name string, _ *metav1.GetOptions,
) (runtime.Object, error) {
	namespace := request.NamespaceValue(ctx)

	var node simplyblockv1alpha2.StorageNode
	err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &node)
	switch {
	case apierrors.IsNotFound(err):
		return nil, apierrors.NewNotFound(metricsv1alpha2.Resource(NodeResourceName), name)
	case err != nil:
		return nil, apierrors.NewInternalError(err)
	}

	lookup := newNodeCapacityLookup(s.reader, s.capacity, logf.FromContext(ctx))
	sample, ok := lookup.forNode(ctx, &node)
	if !ok {
		// The node is real but nothing has measured it: a cold exporter, a
		// Prometheus that cannot be reached, or a node the control plane has not
		// sampled yet.
		return nil, apierrors.NewNotFound(metricsv1alpha2.Resource(NodeResourceName), name)
	}
	return newNodeReading(&node, sample), nil
}

// List implements rest.Lister. It walks the node objects rather than the samples,
// because the objects are what carry a name and a namespace and a sample under
// neither is not servable.
func (s *NodeStorage) List(
	ctx context.Context, options *metainternalversion.ListOptions,
) (runtime.Object, error) {
	namespace := request.NamespaceValue(ctx) // "" for a cluster-wide list
	selector := labels.Everything()
	if options != nil && options.LabelSelector != nil {
		selector = options.LabelSelector
	}

	var nodes simplyblockv1alpha2.StorageNodeList
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := s.reader.List(ctx, &nodes, opts...); err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	lookup := newNodeCapacityLookup(s.reader, s.capacity, logf.FromContext(ctx))

	out := &metricsv1alpha2.StorageNodeMetricsList{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !selector.Matches(labels.Set(node.Labels)) {
			continue
		}
		sample, ok := lookup.forNode(ctx, node)
		if !ok {
			continue
		}
		reading := newNodeReading(node, sample)
		if !matchesNodeFieldSelector(options, reading) {
			continue
		}
		out.Items = append(out.Items, *reading)
	}
	return out, nil
}

// matchesNodeFieldSelector applies the only two field selectors this resource can
// answer. They are supported because a client that passes one and is silently
// ignored gets a wrong answer rather than an error. Anything else selects
// nothing, which is the honest response to a field the object has no index for.
func matchesNodeFieldSelector(
	options *metainternalversion.ListOptions, reading *metricsv1alpha2.StorageNodeMetrics,
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

// nodeCapacityLookup fetches samples once per cluster for the duration of one
// request, because a list walks many nodes of the same few clusters and each
// query is an HTTP round trip. The cluster UUID behind a node's spec.clusterRef
// is memoized for the same reason.
//
// A cluster whose query fails is recorded as having no samples and is not retried
// within the request. Prometheus being down therefore costs the readings and not
// the request.
type nodeCapacityLookup struct {
	reader    client.Reader
	source    NodeCapacitySource
	log       logr.Logger
	uuids     map[client.ObjectKey]string
	byCluster map[string]map[string]prometheus.Capacity
}

func newNodeCapacityLookup(
	reader client.Reader, source NodeCapacitySource, log logr.Logger,
) *nodeCapacityLookup {
	return &nodeCapacityLookup{
		reader:    reader,
		source:    source,
		log:       log,
		uuids:     map[client.ObjectKey]string{},
		byCluster: map[string]map[string]prometheus.Capacity{},
	}
}

// forNode returns the sample for one node and whether there is one. The two cases
// are distinguished, because every field of a node's reading is measured: with no
// sample there is nothing to serve.
func (l *nodeCapacityLookup) forNode(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) (prometheus.Capacity, bool) {
	if l == nil || l.source == nil || node.Status.UUID == "" {
		return prometheus.Capacity{}, false
	}
	clusterUUID := l.clusterUUID(ctx, node)
	if clusterUUID == "" {
		return prometheus.Capacity{}, false
	}
	samples, ok := l.byCluster[clusterUUID]
	if !ok {
		var err error
		samples, err = l.source.NodeCapacity(ctx, clusterUUID)
		if err != nil {
			l.log.V(1).Info("no node capacity samples for this request",
				"cluster", clusterUUID, "err", err.Error())
			samples = nil
		}
		l.byCluster[clusterUUID] = samples
	}
	sample, ok := samples[node.Status.UUID]
	if !ok || !sample.Sampled() {
		return prometheus.Capacity{}, false
	}
	return sample, true
}

// clusterUUID resolves a node's spec.clusterRef to the identifier Prometheus keys
// on. A cluster that cannot be read, or one with no UUID yet, memoizes the empty
// string, so a namespace whose cluster is still being created costs one read for
// the whole request rather than one per node.
func (l *nodeCapacityLookup) clusterUUID(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) string {
	key := client.ObjectKey{Namespace: node.Namespace, Name: node.Spec.ClusterRef}
	if uuid, ok := l.uuids[key]; ok {
		return uuid
	}
	var cluster simplyblockv1alpha2.StorageCluster
	if err := l.reader.Get(ctx, key, &cluster); err != nil {
		l.log.V(1).Info("no cluster for this node's readings",
			"cluster", key.String(), "err", err.Error())
		l.uuids[key] = ""
		return ""
	}
	l.uuids[key] = cluster.Status.UUID
	return cluster.Status.UUID
}

// newNodeReading assembles the served object from the node's own object and the
// last sample taken of it.
func newNodeReading(
	node *simplyblockv1alpha2.StorageNode, sample prometheus.Capacity,
) *metricsv1alpha2.StorageNodeMetrics {
	return &metricsv1alpha2.StorageNodeMetrics{
		TypeMeta: metav1.TypeMeta{
			APIVersion: metricsv1alpha2.GroupVersion.String(),
			Kind:       "StorageNodeMetrics",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              node.Name,
			Namespace:         node.Namespace,
			Labels:            node.Labels,
			CreationTimestamp: node.CreationTimestamp,
		},
		Timestamp:      metav1.NewTime(sample.SampledAt),
		NodeID:         node.Status.UUID,
		StorageCluster: node.Spec.ClusterRef,
		WorkerNode:     node.Spec.WorkerNode,
		Capacity: metricsv1alpha2.StorageNodeCapacity{
			Total:              *resource.NewQuantity(sample.Total, resource.BinarySI),
			Used:               *resource.NewQuantity(sample.Used, resource.BinarySI),
			Free:               *resource.NewQuantity(sample.Free, resource.BinarySI),
			Provisioned:        *resource.NewQuantity(sample.Provisioned, resource.BinarySI),
			UtilizationPercent: sample.UtilizationPercent,
		},
	}
}

// nodeColumns are the table headers, in print order. What they answer: which
// node, on which worker, in which cluster, how big it is, how much of it is in
// use, and how stale the reading is.
var nodeColumns = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name", Description: "The StorageNode this reading is of"},
	{Name: "Worker", Type: "string", Description: "The Kubernetes worker the node runs on"},
	{Name: "Cluster", Type: "string", Description: "The StorageCluster the node belongs to"},
	{Name: "Total", Type: "string", Description: "The storage the node's devices provide"},
	{Name: "Used", Type: "string", Description: "The space they hold"},
	{Name: "Used%", Type: "string", Description: "The control plane's utilization figure"},
	{Name: "Node", Type: "string", Priority: 1, Description: "The control plane's node UUID"},
	{Name: "Sampled", Type: "string", Description: "How long ago the control plane took the reading"},
}

// ConvertToTable implements rest.TableConvertor for both a single reading and a
// list of them.
func (s *NodeStorage) ConvertToTable(
	_ context.Context, object runtime.Object, _ runtime.Object,
) (*metav1.Table, error) {
	table := &metav1.Table{ColumnDefinitions: nodeColumns}

	switch typed := object.(type) {
	case *metricsv1alpha2.StorageNodeMetrics:
		table.Rows = append(table.Rows, nodeRow(typed))
	case *metricsv1alpha2.StorageNodeMetricsList:
		table.ResourceVersion = typed.ResourceVersion
		for i := range typed.Items {
			table.Rows = append(table.Rows, nodeRow(&typed.Items[i]))
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

func nodeRow(reading *metricsv1alpha2.StorageNodeMetrics) metav1.TableRow {
	return metav1.TableRow{
		Cells: []any{
			reading.Name,
			reading.WorkerNode,
			reading.StorageCluster,
			reading.Capacity.Total.String(),
			reading.Capacity.Used.String(),
			fmt.Sprintf("%d%%", reading.Capacity.UtilizationPercent),
			reading.NodeID,
			translateSampleAge(reading.Timestamp),
		},
		Object: runtime.RawExtension{Object: reading},
	}
}

var (
	_ rest.Storage              = (*NodeStorage)(nil)
	_ rest.Scoper               = (*NodeStorage)(nil)
	_ rest.Getter               = (*NodeStorage)(nil)
	_ rest.Lister               = (*NodeStorage)(nil)
	_ rest.TableConvertor       = (*NodeStorage)(nil)
	_ rest.ShortNamesProvider   = (*NodeStorage)(nil)
	_ rest.SingularNameProvider = (*NodeStorage)(nil)
)
