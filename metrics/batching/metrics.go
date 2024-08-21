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
	batchItemsEmittedCount metric.Float64Counter
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
	batchItemsEmittedCount, err = meter.Float64Counter(metricName("batch_items_emitted_total"), api.WithDescription("total number of batch items emitted by tattler"))
	if err != nil {
		return err
	}
	// should this be a histogram or gauge?
	batchAgeSeconds, err = meter.Float64Histogram(
		metricName("batch_age_seconds"),
		api.WithDescription("age of batch when emitted"),
		api.WithExplicitBucketBoundaries(64, 128, 256, 512, 1024, 2048, 4096),
		// 	Buckets: []float64{0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 1.25, 1.5, 2, 3,
		// 		4, 5, 6, 8, 10, 15, 20, 30, 45, 60},
	)
	return nil
}

// RecordSendEventSuccess increases the eventSentCount metric with success == true
// and records the latency
func RecordBatchEmitted(ctx context.Context, sourceType data.SourceType,batchItemCount int, elapsed time.Duration) {
	opt := api.WithAttributes(
		attribute.Key(sourceTypeLabel).String(sourceType.String()),
	)
	batchesEmittedCount.Add(ctx, 1, opt)
	batchItemsEmittedCount.Add(ctx, float64(batchItemCount), opt)
	batchAgeSeconds.Record(ctx, elapsed.Seconds(), opt)
}
