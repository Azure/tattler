package watchlist

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/watch"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/tattler/data"
)

var serviceName attribute.KeyValue

// Based on
// https://github.com/open-telemetry/opentelemetry-go/blob/c609b12d9815bbad0810d67ee0bfcba0591138ce/exporters/prometheus/exporter_test.go
func TestWatchListMetrics(t *testing.T) {
	testCases := []struct {
		name               string
		emptyResource      bool
		customResouceAttrs []attribute.KeyValue
		recordMetrics      func(ctx context.Context, meter otelmetric.Meter)
		options            []otelprometheus.Option
		expectedFile       string
	}{
		{
			name:         "batching metrics",
			expectedFile: "testdata/watchlist_happy.txt",
			recordMetrics: func(ctx context.Context, meter otelmetric.Meter) {
				Init(meter)
				events := []watch.Event{
					{
						Type:   watch.Added,
					},
					{
						Type:   watch.Error,
					},
				}
				for _, event := range events {
					RecordWatchEvent(ctx, event, 1 * time.Second)
					RecordDataEntry(ctx, data.MustNewEntry(&corev1.Node{}, data.STInformer, data.CTAdd))
					RecordDataEntry(ctx, data.MustNewEntry(&corev1.Pod{}, data.STInformer, data.CTAdd))
					RecordDataEntry(ctx, data.MustNewEntry(&corev1.Pod{}, data.STInformer, data.CTUpdate))
					RecordDataEntry(ctx, data.MustNewEntry(&corev1.Pod{}, data.STInformer, data.CTUpdate))
					RecordDataEntry(ctx, data.MustNewEntry(&corev1.Pod{}, data.STInformer, data.CTDelete))
				}
			},
		},
		{
			name:         "batching metrics not initialized",
			expectedFile: "testdata/watchlist_nometrics.txt",
			recordMetrics: func(ctx context.Context, meter otelmetric.Meter) {
				RecordWatchEvent(ctx, watch.Event{Type: watch.Added}, 1 * time.Second)
				RecordDataEntry(ctx, data.MustNewEntry(&corev1.Node{}, data.STInformer, data.CTAdd))
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			registry := prometheus.NewRegistry()
			exporter, err := otelprometheus.New(append(tc.options, otelprometheus.WithRegisterer(registry))...)
			require.NoError(t, err)

			var res *resource.Resource
			if tc.emptyResource {
				res = resource.Empty()
			} else {
				res, err = resource.New(ctx,
					// always specify service.name because the default depends on the running OS
					resource.WithAttributes(semconv.ServiceName("tattler_test")),
					// Overwrite the semconv.TelemetrySDKVersionKey value so we don't need to update every version
					resource.WithAttributes(semconv.TelemetrySDKVersion("latest")),
					resource.WithAttributes(tc.customResouceAttrs...),
				)
				require.NoError(t, err)

				res, err = resource.Merge(resource.Default(), res)
				require.NoError(t, err)
			}

			provider := metric.NewMeterProvider(
				metric.WithResource(res),
				metric.WithReader(exporter),
				metric.WithView(metric.NewView(
					metric.Instrument{Name: "histogram_*"},
					metric.Stream{Aggregation: metric.AggregationExplicitBucketHistogram{
						Boundaries: []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 1000},
					}},
				)),
			)
			meter := provider.Meter(
				"testmeter",
				otelmetric.WithInstrumentationVersion("v0.1.0"),
			)

			tc.recordMetrics(ctx, meter)

			file, err := os.Open(tc.expectedFile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, file.Close()) })

			err = testutil.GatherAndCompare(registry, file)
			require.NoError(t, err)
		})
	}
}
