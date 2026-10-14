package readers

import (
	"testing"

	"github.com/Azure/tattler/data"
	"github.com/gostdlib/base/context"
	"github.com/kylelemons/godebug/pretty"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel/attribute"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// Based on
// https://github.com/open-telemetry/opentelemetry-go/blob/c609b12d9815bbad0810d67ee0bfcba0591138ce/exporters/prometheus/exporter_test.go
func TestWatchListMetrics(t *testing.T) {
	// Not parallel: it resets and re-initializes the package-level metrics.

	tests := []struct {
		name               string
		emptyResource      bool
		wantMetricCount    int
		customResouceAttrs []attribute.KeyValue
		recordMetrics      func(ctx context.Context, meter otelmetric.Meter)
		options            []otelprometheus.Option
	}{
		{
			name: "Success: initialized metrics record a watch event and a data entry",
			recordMetrics: func(ctx context.Context, meter otelmetric.Meter) {
				// Init only binds the counters once per process, so without this a repeated run records to the meter
				// from the first run.
				testResetOnce()
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
			name: "Success: uninitialized metrics record nothing",
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

// recorded initializes the package metrics against a fresh reader, runs record and returns the attributes of every data
// point recorded to the metric named name, as sorted "key=value" strings.
func recorded(t *testing.T, name string, record func(ctx context.Context)) [][]string {
	reader := metric.NewManualReader()
	testResetOnce()
	if err := Init(metric.NewMeterProvider(metric.WithReader(reader)).Meter("test")); err != nil {
		t.Fatalf("Init() got err == %s, want err == nil", err)
	}
	record(t.Context())

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("Collect() got err == %s, want err == nil", err)
	}
	var got [][]string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %s got data %T, want metricdata.Sum[int64]", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				var attrs []string
				for _, kv := range dp.Attributes.ToSlice() {
					attrs = append(attrs, string(kv.Key)+"="+kv.Value.Emit())
				}
				got = append(got, attrs)
			}
		}
	}
	return got
}

func TestWatchEvent(t *testing.T) {
	// Not parallel: it resets and re-initializes the package-level metrics.

	tests := []struct {
		name  string
		event watch.Event
		want  [][]string
	}{
		{
			name:  "Success: a known event type uses the attributes built in Init",
			event: watch.Event{Type: watch.Modified},
			want:  [][]string{{"event_type=MODIFIED"}},
		},
		{
			name:  "Success: an unknown event type builds its attributes",
			event: watch.Event{Type: watch.EventType("CUSTOM")},
			want:  [][]string{{"event_type=CUSTOM"}},
		},
	}

	for _, test := range tests {
		got := recorded(t, metricName("watch_event_total"), func(ctx context.Context) { WatchEvent(ctx, test.event) })
		if diff := pretty.Compare(test.want, got); diff != "" {
			t.Errorf("TestWatchEvent(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}

func TestDataEntry(t *testing.T) {
	// Not parallel: it resets and re-initializes the package-level metrics.

	tests := []struct {
		name  string
		entry data.Entry
		want  [][]string
	}{
		{
			name:  "Success: a watchlist pod update",
			entry: data.MustNewEntry(&corev1.Pod{}, data.STWatchList, data.CTUpdate),
			want:  [][]string{{"change_type=Update", "keep=true", "object_type=Pod", "source_type=WatchList"}},
		},
		{
			name:  "Success: the last value of every type, which sits at the edge of the table built in Init",
			entry: data.MustNewEntry(&corev1.Endpoints{}, data.STEtcd, data.CTSnapshot),
			want:  [][]string{{"change_type=Snapshot", "keep=true", "object_type=Endpoint", "source_type=Etcd"}},
		},
	}

	for _, test := range tests {
		got := recorded(t, metricName("data_entry_total"), func(ctx context.Context) { DataEntry(ctx, test.entry) })
		if diff := pretty.Compare(test.want, got); diff != "" {
			t.Errorf("TestDataEntry(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}
