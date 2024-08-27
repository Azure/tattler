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
	sourceTypeLabel = "source_type"
	changeTypeLabel = "change_type"
	objectTypeLabel = "object_type"
)

var (
	watchEventCount metric.Float64Counter
	dataEntryCount  metric.Float64Counter
)

func metricName(name string) string {
	return fmt.Sprintf("%s_%s", subsystem, name)
}

// NewRegistry creates a new Registry with initialized prometheus counter definitions
func Init(meter api.Meter) error {
	var err error
	watchEventCount, err = meter.Float64Counter(metricName("watch_event_total"), api.WithDescription("total number of batches emitted by tattler"))
	if err != nil {
		return err
	}
	dataEntryCount, err = meter.Float64Counter(metricName("data_entry_total"), api.WithDescription("total number of batches emitted by tattler"))
	if err != nil {
		return err
	}
	// get object age?
	return nil
}

// RecordWatchEvent increases the watchEventCount metric
// with event type = (added, modified, deleted, bookmark, error)
func RecordWatchEvent(ctx context.Context, e watch.Event) {
	opt := api.WithAttributes(
		// added, modified, deleted, bookmark, error
		attribute.Key(eventTypeLabel).String(string(e.Type)),
	)
	if watchEventCount != nil {
		watchEventCount.Add(ctx, 1, opt)
	}
}

// RecordDataEntry increases the dataEntryCount metric
func RecordDataEntry(ctx context.Context, e data.Entry) {
	opt := api.WithAttributes(
		attribute.Key(sourceTypeLabel).String(e.SourceType().String()),
		attribute.Key(changeTypeLabel).String(e.ChangeType().String()),
		attribute.Key(objectTypeLabel).String(e.ObjectType().String()),
	)
	if dataEntryCount != nil {
		dataEntryCount.Add(ctx, 1, opt)
	}
}
