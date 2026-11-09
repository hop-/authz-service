// Package metricstest reads the service's metrics from the default Prometheus
// registry in tests. It is test-only: no production code imports it.
//
// It works on the gathered metric families directly, so tests need neither
// prometheus/testutil nor its extra module requirements.
package metricstest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// Value returns the value of the series name{labels} on the default registry:
// a counter's or gauge's value, or a histogram's sample count. labels must
// match the series' labels exactly. It returns 0 when no series matches.
func Value(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("metricstest: gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			match := true
			for _, lp := range m.GetLabel() {
				if v, ok := labels[lp.GetName()]; !ok || v != lp.GetValue() {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

// Series returns the label sets of every series of name on the default
// registry, and whether the metric family exists.
func Series(t testing.TB, name string) ([]map[string]string, bool) {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("metricstest: gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var out []map[string]string
		for _, m := range mf.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			out = append(out, labels)
		}
		return out, true
	}
	return nil, false
}

// Type returns the metric type of name ("COUNTER", "GAUGE", "HISTOGRAM", ...)
// on the default registry, or "" when it isn't registered or has no series.
func Type(t testing.TB, name string) string {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("metricstest: gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetType().String()
		}
	}
	return ""
}
