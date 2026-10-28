package readers

import (
	"fmt"
	"sync/atomic"

	"github.com/Azure/tattler/data"
	"github.com/gostdlib/base/concurrency/sync"
	"github.com/gostdlib/base/context"
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

// counters holds the instruments and the attribute options for them. The options are built once in Init, so recording
// an event does not build attribute sets.
type counters struct {
	watchEvent metric.Int64Counter
	dataEntry  metric.Int64Counter

	// watchEventOpts holds the options for each known watch.EventType. Options are kept as slices so passing them to
	// Add does not allocate a new variadic slice per event.
	watchEventOpts map[watch.EventType][]api.AddOption
	// dataEntryOpts is indexed by source type, change type and object type.
	dataEntryOpts [numSourceTypes][numChangeTypes][numObjectTypes][]api.AddOption
}

// These size dataEntryOpts. A type added after the last value here falls back to building its option per event.
const (
	numSourceTypes = int(data.STEtcd) + 1
	numChangeTypes = int(data.CTSnapshot) + 1
	numObjectTypes = int(data.OTEndpoint) + 1
)

// current is nil until Init succeeds, which means metrics are off.
var current atomic.Pointer[counters]

func metricName(name string) string {
	return fmt.Sprintf("%s_%s", subsystem, name)
}

var once sync.Once

// Init initializes the readers metrics. This should only be called by the tattler constructor or tests.
func Init(meter api.Meter) error {
	var err error
	once.Do(
		func() {
			err = doInit(meter)
		},
	)
	return err
}

func doInit(meter api.Meter) error {
	watchEvent, err := meter.Int64Counter(metricName("watch_event_total"), api.WithDescription("total number of watch events handled by tattler"))
	if err != nil {
		return err
	}
	dataEntry, err := meter.Int64Counter(metricName("data_entry_total"), api.WithDescription("total number of data events handled by tattler filter"))
	if err != nil {
		return err
	}

	c := &counters{
		watchEvent:     watchEvent,
		dataEntry:      dataEntry,
		watchEventOpts: map[watch.EventType][]api.AddOption{},
	}
	for _, et := range []watch.EventType{watch.Added, watch.Modified, watch.Deleted, watch.Bookmark, watch.Error} {
		c.watchEventOpts[et] = watchEventOpt(et)
	}
	for st := 0; st < numSourceTypes; st++ {
		for ct := 0; ct < numChangeTypes; ct++ {
			for ot := 0; ot < numObjectTypes; ot++ {
				c.dataEntryOpts[st][ct][ot] = dataEntryOpt(data.SourceType(st), data.ChangeType(ct), data.ObjectType(ot))
			}
		}
	}
	current.Store(c)
	return nil
}

func testResetOnce() {
	once = sync.Once{}
}

// WatchEvent increases the watch_event_total metric
// with event type = (added, modified, deleted, bookmark, error).
func WatchEvent(ctx context.Context, e watch.Event) {
	c := current.Load()
	if c == nil {
		return
	}
	opts, ok := c.watchEventOpts[e.Type]
	if !ok {
		opts = watchEventOpt(e.Type)
	}
	c.watchEvent.Add(ctx, 1, opts...)
}

// DataEntry increases the data_entry_total metric when the data is kept by the filter.
func DataEntry(ctx context.Context, e data.Entry) {
	c := current.Load()
	if c == nil {
		return
	}
	st, ct, ot := e.SourceType(), e.ChangeType(), e.ObjectType()
	var opts []api.AddOption
	switch {
	case int(st) < numSourceTypes && int(ct) < numChangeTypes && int(ot) < numObjectTypes:
		opts = c.dataEntryOpts[st][ct][ot]
	default:
		opts = dataEntryOpt(st, ct, ot)
	}
	c.dataEntry.Add(ctx, 1, opts...)
}

// watchEventOpt builds the attributes recorded for a watch event of type et.
func watchEventOpt(et watch.EventType) []api.AddOption {
	// added, modified, deleted, bookmark, error
	return []api.AddOption{api.WithAttributeSet(attribute.NewSet(attribute.Key(eventTypeLabel).String(string(et))))}
}

// dataEntryOpt builds the attributes recorded for a kept data entry.
func dataEntryOpt(st data.SourceType, ct data.ChangeType, ot data.ObjectType) []api.AddOption {
	set := attribute.NewSet(
		attribute.Key(keepLabel).String("true"),
		attribute.Key(sourceTypeLabel).String(st.String()),
		attribute.Key(changeTypeLabel).String(ct.String()),
		attribute.Key(objectTypeLabel).String(ot.String()),
	)
	return []api.AddOption{api.WithAttributeSet(set)}
}
