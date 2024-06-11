// Package watchlist provides a filter cache that stores the latest version information for objects
// using a map with no locks.
package watchlist

import (
	"context"
	"errors"

	"github.com/Azure/tattler/internal/filter/items"
	"github.com/gostdlib/concurrency/prim/wait"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// Filter provides a data filter cache for filtering out events that are older than the current version.
type Filter struct {
	in  chan watch.Event
	out chan watch.Event
	m   map[types.UID]items.Item
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

// New creates a new Filter. Closing the input channel will stop the Filter.
func New(ctx context.Context, in, out chan watch.Event, options ...Option) (*Filter, error) {
	ctx = context.WithoutCancel(ctx)

	cc := &Filter{
		in:  in,
		out: out,
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
		cc.run()
		return nil
	})

	go func() {
		g.Wait(ctx)
		close(cc.out)
	}()

	return cc, nil
}

// run is the main loop of the Checker.
func (c *Filter) run() {
	for event := range c.in {
		c.handleEvent(event)
	}
}

// handleEvent looks at the event type and acts accordingly.
func (c *Filter) handleEvent(event watch.Event) {
	obj := event.Object.(items.Object)
	if event.Type == watch.Deleted {
		delete(c.m, obj.GetUID())
		c.out <- event
		return
	}

	if c.setMapItem(obj) {
		c.out <- event
	}
}

// setMapItem sets the item in the map if it doesn't exist or if the new pod is newer.
// Returns true if the item was set, false if the item was not set.
func (c *Filter) setMapItem(newItem items.Object) (cached bool) {
	oldItem, ok := c.m[newItem.GetUID()]
	if !ok {
		c.m[newItem.GetUID()] = items.New(newItem)
		return true
	}

	// If the new item is older than the cached item, we don't need to update the cache.
	if oldItem.IsNewer(newItem) {
		return false
	}

	// We need to update the item in the map.
	c.m[newItem.GetUID()] = items.New(newItem)
	return true
}
