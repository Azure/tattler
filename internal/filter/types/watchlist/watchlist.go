// Package watchlist provides a filter cache that stores the latest version information for objects
// using a map with no locks.
package watchlist

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/internal/filter/items"
	metrics "github.com/Azure/tattler/internal/metrics/readers"
	"github.com/gostdlib/concurrency/prim/wait"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// Filter provides a data filter cache for filtering out events that are older than the current version.
type Filter struct {
	in  chan watch.Event
	out chan data.Entry
	m   map[types.UID]items.Item

	logger *slog.Logger
}

// Option is a opional argument for New().
type Option func(*Filter) error

// WithSized sets the initial size of the map.
func WithSized(size int) Option {
	return func(c *Filter) error {
		if size < 0 {
			return errors.New("size must be greater than 0")
		}
		c.m = make(map[types.UID]items.Item, size)
		return nil
	}
}

// WithLogger sets the logger to l.
func WithLogger(l *slog.Logger) Option {
	return func(c *Filter) error {
		c.logger = l
		return nil
	}
}

// New creates a new Filter. Closing the input channel will stop the Filter.
func New(ctx context.Context, in chan watch.Event, out chan data.Entry, options ...Option) (*Filter, error) {
	ctx = context.WithoutCancel(ctx)

	cc := &Filter{
		in:     in,
		out:    out,
		logger: slog.Default(),
	}

	for _, o := range options {
		if err := o(cc); err != nil {
			return nil, err
		}
	}
	if cc.m == nil {
		cc.m = map[types.UID]items.Item{}
	}

	g := wait.Group{}

	g.Go(ctx, func(ctx context.Context) error {
		cc.run(ctx)
		return nil
	})

	go func() {
		g.Wait(ctx)
		close(cc.out)
	}()

	return cc, nil
}

// run is the main loop of the Checker.
func (c *Filter) run(ctx context.Context) {
	for event := range c.in {
		c.handleEvent(ctx, event)
	}
}

// handleEvent looks at the event type and acts accordingly.
func (c *Filter) handleEvent(ctx context.Context, event watch.Event) {
	// All K8 objects implement items.Object.
	obj := event.Object.(items.Object)

	cachedObject, wasSnapshot, wasDeleted := c.setMapItem(obj, event)
	entry, err := c.eventToEntry(event, wasSnapshot)
	if err != nil {
		c.logger.Warn(err.Error())
		return
	}

	if cachedObject || wasSnapshot || wasDeleted {
		metrics.RecordDataEntry(ctx, entry)
		c.out <- entry
	}
}

// eventToEntry converts a watch.Event to a data.Entry.
func (c *Filter) eventToEntry(event watch.Event, wasSnapShot bool) (data.Entry, error) {
	var e data.Entry
	var err error

	if wasSnapShot {
		e, err = data.NewEntry(event.Object, data.STWatchList, data.CTSnapshot)
	} else {
		switch event.Type {
		case watch.Added:
			e, err = data.NewEntry(event.Object, data.STWatchList, data.CTAdd)
		case watch.Modified:
			e, err = data.NewEntry(event.Object, data.STWatchList, data.CTUpdate)
		case watch.Deleted:
			e, err = data.NewEntry(event.Object, data.STWatchList, data.CTDelete)
		default:
			err = fmt.Errorf("unknown event type: %v", event.Type)
		}
	}
	return e, err
}

// setMapItem sets the item in the map if it doesn't exist or if the new pod is newer.
// cached is set to true if we cached the item. If the item was already in the cache but
// had the same value, isSnapshot is true. If the item is indicating a deletion, isDeleted is true
// and the item is deleted from the cache.
func (c *Filter) setMapItem(newItem items.Object, event watch.Event) (cached, isSnapshot, isDeleted bool) {
	if event.Type == watch.Deleted {
		delete(c.m, newItem.GetUID())
		return false, false, true
	}

	oldItem, ok := c.m[newItem.GetUID()]
	if !ok {
		c.m[newItem.GetUID()] = items.New(newItem)
		return true, false, false
	}

	// If the new item is older than the cached item, we don't need to update the cache.
	switch oldItem.IsState(newItem) {
	// This happens when we snapshot objects, we don't need to set the item in the map.
	case items.Equal:
		return false, true, false
	// This happens when the new item is older than the cached item.
	case items.Newer:
		return false, false, false
	}

	// We need to update the item in the map.
	c.m[newItem.GetUID()] = items.New(newItem)
	return true, false, false
}
