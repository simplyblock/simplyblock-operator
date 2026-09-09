package main

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PromAPI fakes the Prometheus HTTP API well enough for charts: instant
// queries return a vector with one sample per storage node, range queries a
// smooth seeded random walk per node. The walk is a pure function of
// (metric, instance, seed, time), so a chart that refreshes sees a stable,
// continuous series rather than noise.
type PromAPI struct {
	store *Store
	seed  uint64
}

var metricNameRe = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

func (p *PromAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		p.error(w, "bad_data", err.Error())
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/query_range"):
		p.queryRange(w, r)
	case strings.HasSuffix(r.URL.Path, "/query"):
		p.query(w, r)
	case strings.HasSuffix(r.URL.Path, "/labels"):
		p.success(w, []string{"__name__", "instance", "node", "cluster", "pool", "volume"})
	case strings.Contains(r.URL.Path, "/label/"):
		p.success(w, p.metricNames())
	case strings.HasSuffix(r.URL.Path, "/series"):
		p.success(w, []any{})
	case strings.HasSuffix(r.URL.Path, "/rules"), strings.HasSuffix(r.URL.Path, "/alerts"):
		p.success(w, map[string]any{"groups": []any{}, "alerts": []any{}})
	default:
		p.error(w, "bad_data", "unsupported endpoint "+r.URL.Path)
	}
}

func (p *PromAPI) metricNames() []string {
	return []string{
		"sb_node_read_iops", "sb_node_write_iops", "sb_node_read_bytes", "sb_node_write_bytes",
		"sb_node_latency_us", "sb_cluster_capacity_bytes", "sb_cluster_used_bytes",
		"sb_pool_provisioned_bytes", "sb_volume_read_iops", "sb_volume_write_iops",
	}
}

// instances returns the identities series are labeled with, from the store's
// actual topology so charts and tables cross-reference.
func (p *PromAPI) instances() []map[string]string {
	d, ok := p.store.Lookup(sbGroup, "v1alpha1", "storagenodes")
	if !ok {
		return nil
	}
	nodes, _ := p.store.List(d, "", "", "")
	var out []map[string]string
	for _, n := range nodes {
		out = append(out, map[string]string{
			"instance": getStr(n, "status.hostname"),
			"node":     getStr(n, "status.uuid"),
		})
	}
	if len(out) == 0 {
		out = []map[string]string{{"instance": "mock-node-1", "node": "00000000-0000-4000-8000-000000000001"}}
	}
	return out
}

// valueAt is the deterministic series function: a base magnitude by metric
// category, plus two slow sine components and jitter, all seeded per
// (metric, instance).
func (p *PromAPI) valueAt(metric, instance string, t time.Time) float64 {
	h := fnv.New64a()
	h.Write([]byte(metric + "|" + instance))
	hs := h.Sum64() ^ p.seed
	rng := rand.New(rand.NewPCG(hs, hs>>7))

	base, span := 1000.0, 800.0
	switch {
	case strings.Contains(metric, "latency"), strings.Contains(metric, "seconds"), strings.Contains(metric, "_us"):
		base, span = 180, 120
	case strings.Contains(metric, "bytes") && strings.Contains(metric, "capacity"):
		return 7.68e12 + float64(hs%4)*1.92e12 // capacities are flat lines
	case strings.Contains(metric, "bytes"):
		base, span = 4.2e8, 3.5e8
	case strings.Contains(metric, "iops"), strings.Contains(metric, "ops"):
		base, span = 42000, 30000
	case strings.Contains(metric, "util"), strings.Contains(metric, "percent"):
		base, span = 55, 30
	}
	base *= 0.6 + rng.Float64()*0.8
	phase := rng.Float64() * 2 * math.Pi
	x := float64(t.Unix())
	v := base +
		span*0.5*math.Sin(x/600+phase) +
		span*0.25*math.Sin(x/91+phase*3) +
		span*0.1*math.Sin(x/13+phase*7)
	if v < 0 {
		v = 0
	}
	return v
}

func (p *PromAPI) query(w http.ResponseWriter, r *http.Request) {
	metric := firstMetric(r.Form.Get("query"))
	t := parsePromTime(r.Form.Get("time"), time.Now())
	var result []any
	for _, inst := range p.instances() {
		labels := map[string]string{"__name__": metric}
		for k, v := range inst {
			labels[k] = v
		}
		result = append(result, map[string]any{
			"metric": labels,
			"value":  []any{float64(t.Unix()), fmt.Sprintf("%.2f", p.valueAt(metric, inst["instance"], t))},
		})
	}
	p.success(w, map[string]any{"resultType": "vector", "result": result})
}

func (p *PromAPI) queryRange(w http.ResponseWriter, r *http.Request) {
	metric := firstMetric(r.Form.Get("query"))
	end := parsePromTime(r.Form.Get("end"), time.Now())
	start := parsePromTime(r.Form.Get("start"), end.Add(-time.Hour))
	step := parsePromDuration(r.Form.Get("step"), 30*time.Second)
	if step <= 0 {
		step = 30 * time.Second
	}
	// cap the number of points so a bad step cannot melt the browser
	if int(end.Sub(start)/step) > 2000 {
		step = end.Sub(start) / 2000
	}
	var result []any
	for _, inst := range p.instances() {
		labels := map[string]string{"__name__": metric}
		for k, v := range inst {
			labels[k] = v
		}
		var values []any
		for t := start; !t.After(end); t = t.Add(step) {
			values = append(values, []any{float64(t.Unix()), fmt.Sprintf("%.2f", p.valueAt(metric, inst["instance"], t))})
		}
		result = append(result, map[string]any{"metric": labels, "values": values})
	}
	p.success(w, map[string]any{"resultType": "matrix", "result": result})
}

func firstMetric(q string) string {
	if m := metricNameRe.FindString(q); m != "" && m != "rate" && m != "sum" && m != "avg" && m != "irate" && m != "max" && m != "min" && m != "by" {
		return m
	}
	// skip aggregation keywords: take the first identifier that looks like a metric
	for _, m := range metricNameRe.FindAllString(q, -1) {
		switch m {
		case "rate", "irate", "sum", "avg", "max", "min", "by", "on", "group_left", "histogram_quantile":
			continue
		default:
			return m
		}
	}
	return "sb_mock_metric"
}

func parsePromTime(s string, fallback time.Time) time.Time {
	if s == "" {
		return fallback
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		sec := int64(f)
		return time.Unix(sec, int64((f-float64(sec))*1e9))
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return fallback
}

func parsePromDuration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second))
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return fallback
}

func (p *PromAPI) success(w http.ResponseWriter, data any) {
	writeJSON(w, 200, map[string]any{"status": "success", "data": data})
}

func (p *PromAPI) error(w http.ResponseWriter, typ, msg string) {
	writeJSON(w, 400, map[string]any{"status": "error", "errorType": typ, "error": msg})
}
