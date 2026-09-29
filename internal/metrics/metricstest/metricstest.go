// Package metricstest records Octomaton's metrics in memory so tests can read them.
package metricstest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"octomaton.dev/internal/metrics"
)

// Metrics are metrics.Metrics whose recorded values tests can read.
type Metrics struct {
	*metrics.Metrics
	reader *sdkmetric.ManualReader
}

// New returns metrics recorded in memory.
func New(t testing.TB) *Metrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	m, err := metrics.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	return &Metrics{Metrics: m, reader: reader}
}

// Count returns the value of counter name for exactly the given attributes (0 when never counted).
func (m *Metrics) Count(t testing.TB, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()
	return m.value(t, name, attrs, func(data metricdata.Aggregation) []metricdata.DataPoint[int64] {
		sum, _ := data.(metricdata.Sum[int64])
		return sum.DataPoints
	})
}

// Gauge returns the last value of gauge name for exactly the given attributes (0 when never set).
func (m *Metrics) Gauge(t testing.TB, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()
	return m.value(t, name, attrs, func(data metricdata.Aggregation) []metricdata.DataPoint[int64] {
		gauge, _ := data.(metricdata.Gauge[int64])
		return gauge.DataPoints
	})
}

func (m *Metrics) value(t testing.TB, name string, attrs []attribute.KeyValue, points func(metricdata.Aggregation) []metricdata.DataPoint[int64]) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := m.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	want := attribute.NewSet(attrs...)
	for _, scope := range rm.ScopeMetrics {
		for _, md := range scope.Metrics {
			if md.Name != name {
				continue
			}
			for _, point := range points(md.Data) {
				if point.Attributes.Equals(&want) {
					return point.Value
				}
			}
		}
	}
	return 0
}
