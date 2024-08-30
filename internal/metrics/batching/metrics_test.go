package batching

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"go.opentelemetry.io/otel/attribute"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/Azure/tattler/data"
)

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
		expectedFile       string
	}{
		{
			name:         "batching metrics",
			expectedFile: "testdata/batching_happy.txt",
			recordMetrics: func(ctx context.Context, meter otelmetric.Meter) {
				Init(meter)
				Emitted(ctx, data.STWatchList, 3, 1*time.Second)
				Success(ctx)
				Emitted(ctx, data.STWatchList, 1, 4*time.Second)
				Success(ctx)
				Emitted(ctx, data.STInformer, 1, 1*time.Second)
				Success(ctx)
				Error(ctx)
			},
		},
		{
			name:         "batching metrics not initialized",
			expectedFile: "testdata/batching_nometrics.txt",
			recordMetrics: func(ctx context.Context, meter otelmetric.Meter) {
				Emitted(context.Background(), data.STWatchList, 3, 1*time.Second)
				Success(ctx)
				Error(ctx)
			},
		},
	}

	for _, test := range tests {
		log.Println("test: ", test.name)
		ctx := context.Background()
		registry := prometheus.NewRegistry()
		exporter, err := otelprometheus.New(append(test.options, otelprometheus.WithRegisterer(registry))...)
		if err != nil {
			t.Fatalf("failed to create prometheus exporter: %v", err)
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
				t.Fatalf("failed to create resource: %v", err)
			}

			res, err = resource.Merge(resource.Default(), res)
			if err != nil {
				t.Fatalf("failed to merge resources: %v", err)
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

		file, err := os.Open(test.expectedFile)
		if err != nil {
			t.Fatalf("failed to open file: %v", err)
		}
		t.Cleanup(func() {
			if err := file.Close(); err != nil {
				t.Fatalf("failed to close file: %v", err)
			}
		})

		err = testutil.GatherAndCompare(registry, file)
		if err != nil {
			t.Errorf("comparision with metrics file failed: %v", err)
		}
	}
}
