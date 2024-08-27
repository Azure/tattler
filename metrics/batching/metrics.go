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
	batchesEmittedCount    metric.Float64Counter
	batchItemsEmittedCount metric.Float64Counter
	batchAgeSeconds        metric.Float64Histogram
)

func metricName(name string) string {
	return fmt.Sprintf("%s_%s", subsystem, name)
}

// Init initialized the batching metrics.
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
		api.WithExplicitBucketBoundaries(0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 1.25, 1.5, 2, 3, 4, 5),
	)

	return nil
}

// RecordBatchEmitted should be called when emitting a batch to record the batch emitted count, batch item emitted count,
// and time elapsed from when the batch was created.
func RecordBatchEmitted(ctx context.Context, sourceType data.SourceType, batchItemCount int, elapsed time.Duration) {
	opt := api.WithAttributes(
		attribute.Key(sourceTypeLabel).String(sourceType.String()),
	)
	// check if initialized first so someone doesn't have to initialize metrics
	if batchesEmittedCount != nil {
		batchesEmittedCount.Add(ctx, 1, opt)
	}
	if batchItemsEmittedCount != nil {
		batchItemsEmittedCount.Add(ctx, float64(batchItemCount), opt)
	}
	if batchAgeSeconds != nil {
		batchAgeSeconds.Record(ctx, elapsed.Seconds(), opt)
	}
}
