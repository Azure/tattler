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
	sourceTypeLabel = "source_type"
	successLabel    = "success"
)

var (
	batchingCount          metric.Int64Counter
	batchesEmittedCount    metric.Int64Counter
	batchItemsEmittedCount metric.Int64Counter
	batchAgeSeconds        metric.Float64Histogram
)

func metricName(name string) string {
	return fmt.Sprintf("%s_%s", subsystem, name)
}

// Init initializes the batching metrics.
func Init(meter api.Meter) error {
	var err error
	batchingCount, err = meter.Int64Counter(metricName("batching_total"), api.WithDescription("total number of times tattler handles batching input"))
	if err != nil {
		return err
	}
	batchesEmittedCount, err = meter.Int64Counter(metricName("batches_emitted_total"), api.WithDescription("total number of batches emitted by tattler"))
	if err != nil {
		return err
	}
	batchItemsEmittedCount, err = meter.Int64Counter(metricName("batch_items_emitted_total"), api.WithDescription("total number of batch items emitted by tattler"))
	if err != nil {
		return err
	}
	batchAgeSeconds, err = meter.Float64Histogram(
		metricName("batch_age_seconds"),
		api.WithDescription("age of batch when emitted"),
		api.WithExplicitBucketBoundaries(0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 1.25, 1.5, 2, 3, 4, 5),
	)

	return nil
}

// RecordBatchingSuccess records successful handling of batch input
// so we can calculate error rate.
func RecordBatchingSuccess(ctx context.Context) {
	opt := api.WithAttributes(
		attribute.Key(successLabel).String("true"),
	)
	if batchingCount != nil {
		batchingCount.Add(ctx, 1, opt)
	}
}

// RecordBatchingError records an error when handling batch input.
func RecordBatchingError(ctx context.Context) {
	opt := api.WithAttributes(
		attribute.Key(successLabel).String("false"),
	)
	if batchingCount != nil {
		batchingCount.Add(ctx, 1, opt)
	}
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
		batchItemsEmittedCount.Add(ctx, int64(batchItemCount), opt)
	}
	if batchAgeSeconds != nil {
		batchAgeSeconds.Record(ctx, elapsed.Seconds(), opt)
	}
}
