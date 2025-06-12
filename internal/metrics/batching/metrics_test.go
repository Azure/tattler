package batching

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel/attribute"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/Azure/tattler/data"
)

type comparables[T any] struct {
	name  string
	value T
	seen  bool
}

/*
2025/06/11 18:26:13 metric family:  tattler_batch_age_ms
2025/06/11 18:26:13 metric family:  tattler_batch_items_emitted_total
2025/06/11 18:26:13 metric family:  tattler_batches_emitted_total
2025/06/11 18:26:13 metric family:  tattler_batching_total
*/

// Based on
// https://github.com/open-telemetry/opentelemetry-go/blob/c609b12d9815bbad0810d67ee0bfcba0591138ce/exporters/prometheus/exporter_test.go
func TestBatchingMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		emptyResource      bool
		customResouceAttrs []attribute.KeyValue
		recordMetrics      func(ctx context.Context, meter otelmetric.Meter)
		options            []otelprometheus.Option
		wantMetricCount    int
	}{
		{
			name: "batching metrics",
			recordMetrics: func(ctx context.Context, meter otelmetric.Meter) {
				Init(meter)
				Emitted(ctx, data.STWatchList, 1, 1*time.Second)
				Success(ctx)
				Error(ctx)
			},
			wantMetricCount: 6,
		},
		{
			name: "batching metrics not initialized",
			recordMetrics: func(ctx context.Context, meter otelmetric.Meter) {
				Emitted(context.Background(), data.STWatchList, 1, 1*time.Second)
				Success(ctx)
				Error(ctx)
			},
			wantMetricCount: 1,
		},
	}

	for _, test := range tests {
		ctx := context.Background()
		registry := prometheus.NewRegistry()
		exporter, err := otelprometheus.New(append(test.options, otelprometheus.WithRegisterer(registry))...)
		if err != nil {
			t.Fatalf("TestBatchingMetrics(%s): failed to create prometheus exporter: %v", test.name, err)
		}

		var res *resource.Resource
		if test.emptyResource {
			res = resource.Empty()
		} else {
			res, err = resource.New(ctx,
				// always specify service.name because the default depends on the running OS
				resource.WithAttributes(semconv.ServiceName("tattler_test")),
				// Overwrite the semconv.TelemetrySDKVersionKey value so we don't need to update every version
				resource.WithAttributes(semconv.TelemetrySDKVersion("latest")),
				resource.WithAttributes(test.customResouceAttrs...),
			)
			if err != nil {
				t.Fatalf("TestBatchingMetrics(%s)failed to create resource: %v", test.name, err)
			}

			res, err = resource.Merge(resource.Default(), res)
			if err != nil {
				t.Fatalf("TestBatchingMetrics(%s): failed to merge resources: %v", test.name, err)
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

		test.recordMetrics(ctx, meter)

		mfs, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		if want, got := test.wantMetricCount, len(mfs); want != got {
			t.Errorf("unexpected number of metric families gathered, want %d, got %d", want, got)
		}
		for _, mf := range mfs {
			if len(mf.Metric) == 0 {
				t.Errorf("metric family %s must not be empty", mf.GetName())
			}
			for _, m := range mf.Metric {
				switch mf.GetType() {
				case dto.MetricType_COUNTER:
					if m.Counter.GetValue() != 1 {
						t.Errorf("counter metric %s must have value 1, got %f", mf.GetName(), m.Counter.GetValue())
					}
				case dto.MetricType_HISTOGRAM:
					if len(m.Histogram.GetBucket()) < 1 {
						t.Errorf("histogram metric %s must have 1 or more buckets, got %d", mf.GetName(), len(m.Histogram.GetBucket()))
					}
				case dto.MetricType_GAUGE:
					// currently the only tested metrics are gauges with a value of 1
					if m.Gauge.GetValue() != 1 {
						t.Errorf("gauge metric %s must have value 1, got %f", mf.GetName(), m.Gauge.GetValue())
					}
				default:
					t.Errorf("unexpected metric type %s for metric %s", mf.GetType().String(), mf.GetName())
				}
			}
		}
	}
}
