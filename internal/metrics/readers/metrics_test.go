package readers

import (
	"context"
	"testing"

	"github.com/Azure/tattler/data"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel/attribute"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// Based on
// https://github.com/open-telemetry/opentelemetry-go/blob/c609b12d9815bbad0810d67ee0bfcba0591138ce/exporters/prometheus/exporter_test.go
func TestWatchListMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		emptyResource      bool
		wantMetricCount    int
		customResouceAttrs []attribute.KeyValue
		recordMetrics      func(ctx context.Context, meter otelmetric.Meter)
		options            []otelprometheus.Option
	}{
		{
			name: "batching metrics",
			recordMetrics: func(ctx context.Context, meter otelmetric.Meter) {
				Init(meter)
				events := []watch.Event{
					{
						Type: watch.Added,
					},
				}
				for _, event := range events {
					WatchEvent(ctx, event)
					DataEntry(ctx, data.MustNewEntry(&corev1.Pod{}, data.STInformer, data.CTUpdate))
				}
			},
			wantMetricCount: 3,
		},
		{
			name: "batching metrics not initialized",
			recordMetrics: func(ctx context.Context, meter otelmetric.Meter) {
				WatchEvent(ctx, watch.Event{Type: watch.Added})
				DataEntry(ctx, data.MustNewEntry(&corev1.Node{}, data.STInformer, data.CTAdd))
			},
			wantMetricCount: 1,
		},
	}

	for _, test := range tests {
		registry := prometheus.NewRegistry()
		exporter, err := otelprometheus.New(append(test.options, otelprometheus.WithRegisterer(registry))...)
		if err != nil {
			t.Errorf("TestWatchListMetrics(%s) failed to create prometheus exporter: %v", test.name, err)
			continue
		}

		var res *resource.Resource
		if test.emptyResource {
			res = resource.Empty()
		} else {
			res, err = resource.New(
				t.Context(),
				// always specify service.name because the default depends on the running OS
				resource.WithAttributes(semconv.ServiceName("tattler_test")),
				// Overwrite the semconv.TelemetrySDKVersionKey value so we don't need to update every version
				resource.WithAttributes(semconv.TelemetrySDKVersion("latest")),
				resource.WithAttributes(test.customResouceAttrs...),
			)
			if err != nil {
				t.Errorf("TestWatchListMetrics(%s): failed to create resource: %v", test.name, err)
				continue
			}

			res, err = resource.Merge(resource.Default(), res)
			if err != nil {
				t.Errorf("TestWatchListMetrics(%s): failed to merge resources: %v", test.name, err)
				continue
			}
		}

		provider := metric.NewMeterProvider(
			metric.WithResource(res),
			metric.WithReader(exporter),
		)
		meter := provider.Meter(
			"testmeter",
			otelmetric.WithInstrumentationVersion("v0.1.0"),
		)

		test.recordMetrics(t.Context(), meter)

		mfs, err := registry.Gather()
		if err != nil {
			panic(err)
		}
		if want, got := test.wantMetricCount, len(mfs); want != got {
			t.Errorf("TestWatchListMetrics(%s) unexpected number of metric families gathered, want %d, got %d", test.name, want, got)
			continue
		}
		for _, mf := range mfs {
			if len(mf.Metric) == 0 {
				t.Errorf("TestWatchListMetrics(%s) metric family %s must not be empty", test.name, mf.GetName())
				continue
			}
			for _, m := range mf.Metric {
				switch mf.GetType() {
				case dto.MetricType_COUNTER:
					if m.Counter.GetValue() != 1 {
						t.Errorf("TestWatchListMetrics(%s) counter metric %s must have value 1, got %f", test.name, mf.GetName(), m.Counter.GetValue())
					}
				case dto.MetricType_HISTOGRAM:
					if len(m.Histogram.GetBucket()) < 1 {
						t.Errorf("TestWatchListMetrics(%s) histogram metric %s must have 1 or more buckets, got %d", test.name, mf.GetName(), len(m.Histogram.GetBucket()))
					}
				case dto.MetricType_GAUGE:
					// currently the only tested metrics are gauges with a value of 1
					if m.Gauge.GetValue() != 1 {
						t.Errorf("TestWatchListMetrics(%s) gauge metric %s must have value 1, got %f", test.name, mf.GetName(), m.Gauge.GetValue())
					}
				default:
					t.Errorf("TestWatchListMetrics(%s) unexpected metric type %s for metric %s", test.name, mf.GetType().String(), mf.GetName())
				}
			}
		}
	}
}
