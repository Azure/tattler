package batching

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	api "go.opentelemetry.io/otel/metric"

	"github.com/Azure/tattler/data"
)

const (
	subsystem       = "tattler"
	successLabel    = "success"
	sourceTypeLabel = "source_type"
)

var (
	batchesEmittedCount metric.Float64Counter
	currentBatchSize    metric.Float64UpDownCounter
	batchAgeSeconds     metric.Float64Histogram
)

func metricName(name string) string {
	return fmt.Sprintf("%s_%s", subsystem, name)
}

// NewRegistry creates a new Registry with initialized prometheus counter definitions
func Init(meter api.Meter) error {
	var err error
	batchesEmittedCount, err = meter.Float64Counter(metricName("batches_emitted_total"), api.WithDescription("total number of batches emitted by tattler"))
	if err != nil {
		return err
	}
	currentBatchSize, err = meter.Float64UpDownCounter(metricName("current_batch_size"), api.WithDescription("current size of batch emitted by tattler"))
	if err != nil {
		return err
	}
	// should this be a histogram or gauge?
	batchAgeSeconds, err = meter.Float64Histogram(
		metricName("batch_age_seconds"),
		api.WithDescription("age of batch when emitted"),
		api.WithExplicitBucketBoundaries(64, 128, 256, 512, 1024, 2048, 4096),
	)
	// batchItemsEmittedCount: prom.NewCounterVec(prom.CounterOpts{
	// 	Name:      "batch_items_emitted_total",
	// 	Help:      "total number of events sent by the ARN client",
	// 	Subsystem: subsystem,
	// }, []string{successLabel, "source_type"}),
	// eventSentLatency: prom.NewHistogramVec(prom.HistogramOpts{
	// 	Name:      "event_sent_seconds",
	// 	Help:      "latency distributions of events sent by the ARN client",
	// 	Subsystem: subsystem,
	// 	Buckets: []float64{0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 1.25, 1.5, 2, 3,
	// 		4, 5, 6, 8, 10, 15, 20, 30, 45, 60},
	// }, []string{}),
	// // currentBatchSize: prom.NewGaugeVec(prom.GaugeOpts{
	// // 	Name:      "current_batch_size",
	// // 	Help:      "total number of events sent by the ARN client",
	// // 	Subsystem: subsystem,
	// // }, []string{"source_type"}),
	// currentBatchSize: currentBatchSize,
	return nil
}

// RecordSendEventSuccess increases the eventSentCount metric with success == true
// and records the latency
func RecordBatchEmitted(ctx context.Context, sourceType data.SourceType, elapsed time.Duration) {
	opt := api.WithAttributes(
		attribute.Key(sourceTypeLabel).String(sourceType.String()),
	)
	batchesEmittedCount.Add(ctx, 1, opt)
	batchAgeSeconds.Record(ctx, elapsed.Seconds(), opt)
}
