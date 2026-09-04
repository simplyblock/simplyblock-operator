// The Provider itself: the connection to a Prometheus endpoint and the query
// shapes every metric family in this package is built from. They live
// together because the query helpers are the whole of what a Provider is. The
// per-family files decide only which metrics to ask for and how to key the
// answer.

package prometheus

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	prometheusapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// Provider queries one Prometheus endpoint. It holds no state beyond the
// client, so it is safe for concurrent use and cheap enough to construct per
// call site.
type Provider struct {
	api promv1.API
}

// New constructs a Provider against the given Prometheus base URL, for example
// http://simplyblock-prometheus:9090. It performs no I/O: an endpoint that is
// unreachable or absent surfaces on the first query, not here.
func New(prometheusURL string) (*Provider, error) {
	c, err := prometheusapi.NewClient(prometheusapi.Config{Address: prometheusURL})
	if err != nil {
		return nil, fmt.Errorf("create prometheus client: %w", err)
	}
	return &Provider{api: promv1.NewAPI(c)}, nil
}

// NewWithAPI constructs a Provider over an existing Prometheus API client. It
// is the seam a test uses to answer queries without a Prometheus.
func NewWithAPI(api promv1.API) *Provider {
	return &Provider{api: api}
}

// queryVector runs an instant query and requires the result to be a vector,
// which every query in this package is: each metric is a gauge sampled per
// entity, so one series per entity is the shape that comes back.
func (p *Provider) queryVector(ctx context.Context, query string) (model.Vector, error) {
	val, _, err := p.api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("prometheus query %q: %w", query, err)
	}
	vec, ok := val.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("prometheus query %q: unexpected result type %T", query, val)
	}
	return vec, nil
}

// queryFamilyByLabel fetches several metrics in a single query and returns each
// series grouped by the entity it belongs to, then by metric name.
//
// One query rather than one per metric, because a caller assembling a capacity
// sample needs all of them together: six sequential round trips would put six
// scrape-aged values from six different instants into one answer, and on an API
// read path they are six chances to be slow.
//
// The name matcher is an alternation of exact names. PromQL anchors regular
// expressions on both ends, so a name is matched whole and a metric such as
// lvol_size_prov_util is not caught by asking for lvol_size_prov.
func (p *Provider) queryFamilyByLabel(
	ctx context.Context,
	metrics []string,
	idLabel, clusterUUID string,
) (map[string]map[string]float64, error) {
	query := fmt.Sprintf(`{__name__=~%q,cluster=%q}`, strings.Join(metrics, "|"), clusterUUID)
	vec, err := p.queryVector(ctx, query)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]float64)
	for _, sample := range vec {
		id := string(sample.Metric[model.LabelName(idLabel)])
		name := string(sample.Metric[model.MetricNameLabel])
		if id == "" || name == "" {
			continue
		}
		if out[id] == nil {
			out[id] = make(map[string]float64, len(metrics))
		}
		out[id][name] = float64(sample.Value)
	}
	return out, nil
}

// whole rounds a sample to an integer. Prometheus carries every value as a
// float, and these are byte counts, nanosecond latencies, and second-resolution
// timestamps that were integers before they were scraped.
func whole(v float64) int64 { return int64(math.Round(v)) }
