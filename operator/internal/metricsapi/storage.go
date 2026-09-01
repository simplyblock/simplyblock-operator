// The REST storage behind metrics.simplyblock.io/v1alpha1 logicalvolumemetrics:
// the handful of interfaces k8s.io/apiserver asks a resource to implement, wired
// to the control-plane volume cache and the join in binding.go.
//
// There is no etcd behind any of it, which is the reason the package exists, and
// it shows in what is missing: no Create, Update, Delete, or Watch, and no
// continue token on the list. Every read is computed from memory when it
// arrives. What a client may do with the resource is therefore fixed by the
// verbs registered here rather than by RBAC, and the aggregated API server
// rejects the rest before this code is reached.

package metricsapi

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/simplyblock/atlas/lvol"

	metricsv1alpha1 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

// ResourceName is the plural resource these readings are served under. It is
// its own singular as well, the way `endpoints` is: "metrics" is already the
// noun, and "logicalvolumemetric" is not a word.
const ResourceName = "logicalvolumemetrics"

// ShortName is the abbreviation `kubectl get lvm` resolves.
const ShortName = "lvm"

// VolumeSource is the read side of the control-plane volume cache. It is an
// interface so the storage can be tested without a stream, and it is this narrow
// because these two calls are all a read needs.
type VolumeSource interface {
	// All returns every cached volume across every pool.
	All() []subscriptions.VolumeDTO
	// Get returns one cached volume by its control-plane id.
	Get(volumeID string) (subscriptions.VolumeDTO, bool)
}

// Storage serves logicalvolumemetrics. It holds no state of its own: the
// readings come from the volume cache and the identities from the manager's
// Kubernetes cache, and it is the join of the two.
type Storage struct {
	volumes VolumeSource
	reader  client.Reader
}

// NewStorage returns the REST storage. reader must be a cache with
// kube.IndexPVByVolumeHandle registered on PersistentVolumes; without it every
// list is an error rather than a slow answer, which is the failure worth having.
func NewStorage(volumes VolumeSource, reader client.Reader) *Storage {
	return &Storage{volumes: volumes, reader: reader}
}

// New implements rest.Storage.
func (s *Storage) New() runtime.Object { return &metricsv1alpha1.LogicalVolumeMetrics{} }

// Destroy implements rest.Storage. There is nothing to release: no client, no
// watch, and no connection is owned here.
func (s *Storage) Destroy() {}

// NamespaceScoped implements rest.Scoper. The resource is namespaced because
// that is what confines a tenant to their own volumes through ordinary RBAC.
func (s *Storage) NamespaceScoped() bool { return true }

// GetSingularName implements rest.SingularNameProvider.
func (s *Storage) GetSingularName() string { return ResourceName }

// ShortNames implements rest.ShortNamesProvider.
func (s *Storage) ShortNames() []string { return []string{ShortName} }

// NewList implements rest.Lister.
func (s *Storage) NewList() runtime.Object { return &metricsv1alpha1.LogicalVolumeMetricsList{} }

// Get implements rest.Getter. The name is a PersistentVolumeClaim's, so the
// lookup runs claim to volume and never scans the cache.
func (s *Storage) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	namespace := request.NamespaceValue(ctx)
	claim := types.NamespacedName{Namespace: namespace, Name: name}

	bound, ok, err := bindingForClaim(ctx, s.reader, claim)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	if !ok {
		return nil, apierrors.NewNotFound(metricsv1alpha1.Resource(ResourceName), name)
	}

	_, _, volumeID, err := bound.handle.Split()
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("claim %s: %w", claim, err))
	}
	dto, ok := s.volumes.Get(volumeID.String())
	if !ok {
		// The claim is real but the control plane has not reported its volume,
		// which is a cold or disconnected cache. Absent beats a zeroed reading:
		// zeros would be indistinguishable from an empty volume.
		return nil, apierrors.NewNotFound(metricsv1alpha1.Resource(ResourceName), name)
	}
	return newReading(bound, dto), nil
}

// List implements rest.Lister. It walks the volume cache rather than the claims,
// because a volume with no claim is not served at all and the cache is the
// smaller of the two in every cluster this runs on.
func (s *Storage) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	namespace := request.NamespaceValue(ctx) // "" for a cluster-wide list
	selector := labels.Everything()
	if options != nil && options.LabelSelector != nil {
		selector = options.LabelSelector
	}

	out := &metricsv1alpha1.LogicalVolumeMetricsList{}
	for _, dto := range s.volumes.All() {
		handle := lvol.NewVolumeHandle(dto.ClusterID, dto.PoolID, dto.ID)
		bound, ok, err := bindingForHandle(ctx, s.reader, handle)
		if err != nil {
			return nil, apierrors.NewInternalError(err)
		}
		if !ok {
			continue
		}
		if namespace != "" && bound.claim.Namespace != namespace {
			continue
		}
		reading := newReading(bound, dto)
		if !selector.Matches(labels.Set(reading.Labels)) {
			continue
		}
		if !matchesFieldSelector(options, reading) {
			continue
		}
		out.Items = append(out.Items, *reading)
	}
	return out, nil
}

// matchesFieldSelector applies the only two field selectors this resource can
// answer. They are supported because a client that passes one and is silently
// ignored gets a wrong answer rather than an error; anything else selects
// nothing, which is the honest response to a field the object has no index for.
func matchesFieldSelector(options *metainternalversion.ListOptions, reading *metricsv1alpha1.LogicalVolumeMetrics) bool {
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

// newReading assembles the served object from the claim that names it and the
// control plane's last sample of the volume behind it.
func newReading(bound binding, dto subscriptions.VolumeDTO) *metricsv1alpha1.LogicalVolumeMetrics {
	return &metricsv1alpha1.LogicalVolumeMetrics{
		TypeMeta: metav1.TypeMeta{
			APIVersion: metricsv1alpha1.GroupVersion.String(),
			Kind:       "LogicalVolumeMetrics",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              bound.claim.Name,
			Namespace:         bound.claim.Namespace,
			Labels:            bound.labels,
			CreationTimestamp: bound.created,
		},
		Timestamp:        metav1.Unix(dto.Capacity.Date, 0),
		VolumeHandle:     string(bound.handle),
		PersistentVolume: bound.persistentVolume,
		PoolName:         dto.PoolName,
		Capacity: metricsv1alpha1.LogicalVolumeCapacity{
			Provisioned:        *resource.NewQuantity(dto.Capacity.SizeProv, resource.BinarySI),
			Used:               *resource.NewQuantity(dto.Capacity.SizeUsed, resource.BinarySI),
			Free:               *resource.NewQuantity(dto.Capacity.SizeFree, resource.BinarySI),
			Total:              *resource.NewQuantity(dto.Capacity.SizeTotal, resource.BinarySI),
			UtilizationPercent: dto.Capacity.SizeUtil,
		},
	}
}

var (
	_ rest.Storage              = (*Storage)(nil)
	_ rest.Scoper               = (*Storage)(nil)
	_ rest.Getter               = (*Storage)(nil)
	_ rest.Lister               = (*Storage)(nil)
	_ rest.TableConvertor       = (*Storage)(nil)
	_ rest.ShortNamesProvider   = (*Storage)(nil)
	_ rest.SingularNameProvider = (*Storage)(nil)
)
