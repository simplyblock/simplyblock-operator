// Tests for the StoragePoolMetrics REST storage: which pools are served, where
// the numbers come from, and what a namespaced client is confined to.
//
// Two things differ from the device storage next door and are worth reading for.
// A pool's cluster UUID is not on the pool object, so the join runs through the
// StorageCluster, which is one more way for a reading to have no identity. And
// the control plane exports no sample date for a pool, so a served reading
// carries a zero timestamp and "has a reading" cannot be decided by asking the
// sample — which is precisely the mistake that would serve nothing at all.

package metricsapi

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/prometheus"

	metricsv1alpha2 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha2"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testPoolCluster    = "22222222-2222-2222-2222-222222222222"
	testPoolID         = "9b1d3c77-5a02-4f6e-8c31-2e7a4b905fd1"
	testPoolNamespace  = "simplyblock"
	testPoolClusterCR  = "production"
	testPoolObjectName = "tenant-a"
)

// fakePoolCapacity stands in for the metrics endpoint, counting its calls
// because one list must query a cluster once rather than once per pool.
type fakePoolCapacity struct {
	byCluster map[string]map[string]prometheus.Capacity
	err       error
	calls     int
}

func (f *fakePoolCapacity) PoolCapacity(
	_ context.Context, clusterUUID string,
) (map[string]prometheus.Capacity, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byCluster[clusterUUID], nil
}

// poolSample carries no SampledAt, because the control plane exports no
// pool_date. Every pool reading looks like this, which is why presence in the
// query's result is what decides whether there is a reading.
func poolSample() prometheus.Capacity {
	return prometheus.Capacity{
		Total: 10995116277760, Used: 2199023255552, Free: 8796093022208,
		Provisioned: 5497558138880, UtilizationPercent: 20,
	}
}

func storagePool(namespace, name, poolID string) *simplyblockv1alpha2.StoragePool {
	return &simplyblockv1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       simplyblockv1alpha2.StoragePoolSpec{ClusterRef: testPoolClusterCR},
		Status:     simplyblockv1alpha2.StoragePoolStatus{UUID: poolID},
	}
}

// storageCluster is the cluster a pool's UUID is resolved through. Only the
// namespace varies between tests: every pool's clusterRef names the same cluster
// and the samples are keyed by the same UUID, so what a case changes is where the
// cluster is rather than what it is.
func storageCluster(namespace string) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: testPoolClusterCR},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: testPoolCluster},
	}
}

func poolReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		simplyblockv1alpha1.AddToScheme,
		simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build the test scheme: %v", err)
		}
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).Build()
}

func poolSamples() *fakePoolCapacity {
	return &fakePoolCapacity{byCluster: map[string]map[string]prometheus.Capacity{
		testPoolCluster: {testPoolID: poolSample()},
	}}
}

// A pool with an object and a sample is served, and the reading carries both
// halves of the join.
func TestPoolGetServesAPoolWithASample(t *testing.T) {
	storage := NewPoolStorage(poolReader(t,
		storageCluster(testPoolNamespace),
		storagePool(testPoolNamespace, testPoolObjectName, testPoolID),
	), poolSamples())

	ctx := request.WithNamespace(context.Background(), testPoolNamespace)
	object, err := storage.Get(ctx, testPoolObjectName, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	reading, ok := object.(*metricsv1alpha2.StoragePoolMetrics)
	if !ok {
		t.Fatalf("Get returned %T, want a StoragePoolMetrics", object)
	}
	if reading.PoolID != testPoolID {
		t.Errorf("poolID = %q, want %q", reading.PoolID, testPoolID)
	}
	if reading.ClusterID != testPoolCluster {
		t.Errorf("clusterID = %q, want %q", reading.ClusterID, testPoolCluster)
	}
	if got := reading.Capacity.Provisioned.Value(); got != poolSample().Provisioned {
		t.Errorf("capacity.provisioned = %d, want %d", got, poolSample().Provisioned)
	}
	// Absent, not wrong: the control plane exports no date for a pool.
	if !reading.Timestamp.IsZero() {
		t.Errorf("timestamp = %v, want the zero time", reading.Timestamp)
	}
}

// A pool the control plane has not measured is not served. Zeros are the reading
// of an empty pool rather than the absence of a reading.
func TestPoolGetRefusesAPoolWithNoSample(t *testing.T) {
	storage := NewPoolStorage(poolReader(t,
		storageCluster(testPoolNamespace),
		storagePool(testPoolNamespace, testPoolObjectName, "an-unmeasured-pool"),
	), poolSamples())

	ctx := request.WithNamespace(context.Background(), testPoolNamespace)
	_, err := storage.Get(ctx, testPoolObjectName, &metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Get returned %v, want NotFound", err)
	}
}

// A pool whose cluster cannot be resolved has no id to look a sample up by, so
// it is not served rather than served against the wrong cluster's numbers.
func TestPoolGetRefusesAPoolWhoseClusterIsMissing(t *testing.T) {
	storage := NewPoolStorage(poolReader(t,
		storagePool(testPoolNamespace, testPoolObjectName, testPoolID),
	), poolSamples())

	ctx := request.WithNamespace(context.Background(), testPoolNamespace)
	_, err := storage.Get(ctx, testPoolObjectName, &metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Get returned %v, want NotFound", err)
	}
}

// A namespaced list answers with that namespace's pools and no others.
func TestPoolListIsConfinedToItsNamespace(t *testing.T) {
	storage := NewPoolStorage(poolReader(t,
		storageCluster(testPoolNamespace),
		storageCluster("elsewhere"),
		storagePool(testPoolNamespace, testPoolObjectName, testPoolID),
		storagePool("elsewhere", "their-pool", testPoolID),
	), poolSamples())

	ctx := request.WithNamespace(context.Background(), testPoolNamespace)
	object, err := storage.List(ctx, &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	list, ok := object.(*metricsv1alpha2.StoragePoolMetricsList)
	if !ok {
		t.Fatalf("List returned %T, want a StoragePoolMetricsList", object)
	}
	if len(list.Items) != 1 {
		t.Fatalf("List returned %d readings, want just this namespace's", len(list.Items))
	}
	if list.Items[0].Namespace != testPoolNamespace {
		t.Errorf("the reading is from namespace %q, want %q",
			list.Items[0].Namespace, testPoolNamespace)
	}
}

// A list walks many pools of the same cluster and queries it once, because each
// query is an HTTP round trip on a read path.
func TestPoolListQueriesEachClusterOnce(t *testing.T) {
	samples := poolSamples()
	samples.byCluster[testPoolCluster]["second-pool"] = poolSample()
	storage := NewPoolStorage(poolReader(t,
		storageCluster(testPoolNamespace),
		storagePool(testPoolNamespace, testPoolObjectName, testPoolID),
		storagePool(testPoolNamespace, "tenant-b", "second-pool"),
	), samples)

	ctx := request.WithNamespace(context.Background(), testPoolNamespace)
	if _, err := storage.List(ctx, &metainternalversion.ListOptions{}); err != nil {
		t.Fatalf("List: %v", err)
	}

	if samples.calls != 1 {
		t.Errorf("the metrics endpoint was queried %d times for one cluster, want 1", samples.calls)
	}
}

// A deployment with no reachable Prometheus serves no pool readings at all,
// rather than readings of zero.
func TestPoolListServesNothingWithoutASource(t *testing.T) {
	for name, source := range map[string]PoolCapacitySource{
		"no source at all": nil,
		"a failing source": &fakePoolCapacity{err: errors.New("prometheus is unreachable")},
	} {
		t.Run(name, func(t *testing.T) {
			storage := NewPoolStorage(poolReader(t,
				storageCluster(testPoolNamespace),
				storagePool(testPoolNamespace, testPoolObjectName, testPoolID),
			), source)

			ctx := request.WithNamespace(context.Background(), testPoolNamespace)
			object, err := storage.List(ctx, &metainternalversion.ListOptions{})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			list := object.(*metricsv1alpha2.StoragePoolMetricsList)
			if len(list.Items) != 0 {
				t.Errorf("List returned %d readings with no measurements, want none", len(list.Items))
			}
		})
	}
}

// The table is what `kubectl get spm` prints, and Provisioned beside Used is the
// pair the kind exists for: a thin-provisioned pool can be nearly empty and
// fully committed at once, and only one of those two numbers says so.
func TestPoolTableCarriesUsedAndProvisioned(t *testing.T) {
	storage := NewPoolStorage(poolReader(t,
		storageCluster(testPoolNamespace),
		storagePool(testPoolNamespace, testPoolObjectName, testPoolID),
	), poolSamples())

	ctx := request.WithNamespace(context.Background(), testPoolNamespace)
	object, err := storage.Get(ctx, testPoolObjectName, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	table, err := storage.ConvertToTable(ctx, object, nil)
	if err != nil {
		t.Fatalf("ConvertToTable: %v", err)
	}

	if len(table.Rows) != 1 {
		t.Fatalf("the table has %d rows, want 1", len(table.Rows))
	}
	headers := make([]string, 0, len(table.ColumnDefinitions))
	for _, column := range table.ColumnDefinitions {
		headers = append(headers, column.Name)
	}
	for _, want := range []string{"Used", "Provisioned"} {
		found := false
		for _, header := range headers {
			if header == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the table has no %q column: %v", want, headers)
		}
	}
}
