// Small helpers the takeover tests share: a background context, a sentinel
// error, and readers of a gathered Prometheus registry.

package mdsfailover

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type contextT = context.Context

var (
	ctxBackground = context.Background()
	errTest       = errors.New("injected failure")
)

func findMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	next:
		for _, m := range f.GetMetric() {
			have := map[string]string{}
			for _, l := range m.GetLabel() {
				have[l.GetName()] = l.GetValue()
			}
			for k, v := range labels {
				if have[k] != v {
					continue next
				}
			}
			return m
		}
	}
	return nil
}

func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	m := findMetric(t, reg, name, labels)
	switch {
	case m == nil:
		return -1
	case m.Gauge != nil:
		return m.GetGauge().GetValue()
	case m.Counter != nil:
		return m.GetCounter().GetValue()
	}
	return -1
}

func histogramCount(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	m := findMetric(t, reg, name, labels)
	if m == nil || m.Histogram == nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}
