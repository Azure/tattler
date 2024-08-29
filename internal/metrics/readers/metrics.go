package readers

import (
	"context"
	"fmt"

	"github.com/Azure/tattler/data"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	api "go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	subsystem       = "tattler"
	eventTypeLabel  = "event_type"
	keepLabel       = "keep"
	sourceTypeLabel = "source_type"
	changeTypeLabel = "change_type"
	objectTypeLabel = "object_type"
)

var (
	watchEventCount metric.Int64Counter
	dataEntryCount  metric.Int64Counter
	staleDataCount  metric.Int64Counter
)

func metricName(name string) string {
	return fmt.Sprintf("%s_%s", subsystem, name)
}

// Init initializes the readers metrics. This should only be called by the tattler constructor or tests.
func Init(meter api.Meter) error {
	var err error
	watchEventCount, err = meter.Int64Counter(metricName("watch_event_total"), api.WithDescription("total number of watch events handled by tattler"))
	if err != nil {
		return err
	}
	dataEntryCount, err = meter.Int64Counter(metricName("data_entry_total"), api.WithDescription("total number of data events handled by tattler filter"))
	if err != nil {
		return err
	}
	return nil
}

// RecordWatchEvent increases the watchEventCount metric
// with event type = (added, modified, deleted, bookmark, error).
func RecordWatchEvent(ctx context.Context, e watch.Event) {
	opt := api.WithAttributes(
		// added, modified, deleted, bookmark, error
		attribute.Key(eventTypeLabel).String(string(e.Type)),
	)
	if watchEventCount != nil {
		watchEventCount.Add(ctx, 1, opt)
	}
}

// RecordStaleData increases the dataEntryCount metric when the data is stale and dropped by the filter.
func RecordStaleData(ctx context.Context, e data.Entry) {
	opt := api.WithAttributes(
		attribute.Key(keepLabel).String("false"),
		attribute.Key(sourceTypeLabel).String(e.SourceType().String()),
		attribute.Key(sourceTypeLabel).String(e.SourceType().String()),
		attribute.Key(changeTypeLabel).String(e.ChangeType().String()),
		attribute.Key(objectTypeLabel).String(e.ObjectType().String()),
	)
	if dataEntryCount != nil {
		dataEntryCount.Add(ctx, 1, opt)
	}
}

// RecordDataEntry increases the dataEntryCount metric when the data is kept by the filter.
func RecordDataEntry(ctx context.Context, e data.Entry) {
	opt := api.WithAttributes(
		attribute.Key(keepLabel).String("true"),
		attribute.Key(sourceTypeLabel).String(e.SourceType().String()),
		attribute.Key(sourceTypeLabel).String(e.SourceType().String()),
		attribute.Key(changeTypeLabel).String(e.ChangeType().String()),
		attribute.Key(objectTypeLabel).String(e.ObjectType().String()),
	)
	if dataEntryCount != nil {
		dataEntryCount.Add(ctx, 1, opt)
	}
}
