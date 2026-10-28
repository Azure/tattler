package readers

import (
	"testing"

	"github.com/Azure/tattler/data"
	"go.opentelemetry.io/otel/sdk/metric"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func BenchmarkDataEntry(b *testing.B) {
	entry := data.MustNewEntry(&corev1.Pod{}, data.STWatchList, data.CTUpdate)
	ctx := b.Context()

	b.Run("off", func(b *testing.B) {
		testResetOnce()
		clearMetrics()
		b.ReportAllocs()
		for b.Loop() {
			DataEntry(ctx, entry)
		}
	})
	b.Run("on", func(b *testing.B) {
		testResetOnce()
		if err := Init(metric.NewMeterProvider(metric.WithReader(metric.NewManualReader())).Meter("bench")); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			DataEntry(ctx, entry)
		}
	})
}

func BenchmarkWatchEvent(b *testing.B) {
	event := watch.Event{Type: watch.Modified}
	ctx := b.Context()

	b.Run("off", func(b *testing.B) {
		testResetOnce()
		clearMetrics()
		b.ReportAllocs()
		for b.Loop() {
			WatchEvent(ctx, event)
		}
	})
	b.Run("on", func(b *testing.B) {
		testResetOnce()
		if err := Init(metric.NewMeterProvider(metric.WithReader(metric.NewManualReader())).Meter("bench")); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			WatchEvent(ctx, event)
		}
	})
}

// clearMetrics turns metrics off, as if Init was never called.
func clearMetrics() {
	current.Store(nil)
}
