/*
Package routing routes batching.Batches received on an input channel to output receivers
that wish to process the information.

Usage:

	router, err := router.New(ctx, in)
	if err != nil {
		// Do something
	}
	if err := router.Register(ctx, "data handler name", outToCh); err != nil {
		// Do something
	}
	if err := router.Start(ctx); err != nil {
		// Do something
	}

	// Note: closing "in" will stop the router.
*/
package routing

import (
	"fmt"
	"sync/atomic"

	"github.com/Azure/tattler/data"
	metrics "github.com/Azure/tattler/internal/metrics/readers"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/errors"
	"github.com/gostdlib/base/values/chans"
)

type route struct {
	out  chan data.Entry
	name string
	// dropped counts entries dropped because out was full. It is a pointer so copies of the route share it.
	dropped *atomic.Uint64
}

func newRoute(name string, out chan data.Entry) route {
	return route{name: name, out: out, dropped: &atomic.Uint64{}}
}

type routes []route

// Router routes batches to registered destinations.
type Router struct {
	in       chan data.Entry
	routes   routes
	started  bool
	blocking bool
}

// Option configures New. Options are applied in order, so later options override earlier ones. All options are optional; zero values ask
// for defaults.
type Option func(b *Router) error

// WithBlocking makes the router block when pushing data to a route that is not keeping up.
func WithBlocking() Option {
	return func(b *Router) error {
		b.blocking = true
		return nil
	}
}

// New is the constructor for Router.
func New(ctx context.Context, in chan data.Entry, options ...Option) (*Router, error) {
	if in == nil {
		return nil, errors.New("routing.New: input channel cannot be nil")
	}

	b := &Router{
		in:     in,
		routes: routes{},
	}

	for _, o := range options {
		if err := o(b); err != nil {
			return nil, err
		}
	}

	return b, nil
}

// Register registers a routeCh for data with a specific date.EntryType and ObjectType.
// You may register the same combination for different routeCh.
func (b *Router) Register(ctx context.Context, name string, ch chan data.Entry) error {
	if b.started {
		return fmt.Errorf("routing.Batches.Register: cannot Register a route after Start() is called")
	}
	if name == "" {
		return fmt.Errorf("routing.Batches.Register; cannot Register a route with an empty name")
	}
	if ch == nil {
		return fmt.Errorf("routing.Batches.Register: cannot Register a route with a nil channel")
	}

	b.routes = append(b.routes, newRoute(name, ch))
	return nil
}

// Exists returns true if a route with the given name exists.
func (b *Router) Exists(name string) bool {
	for _, r := range b.routes {
		if r.name == name {
			return true
		}
	}
	return false
}

// Start starts routing data coming from input. This can be stopped by closing the input channel.
func (b *Router) Start(ctx context.Context) error {
	if len(b.routes) == 0 {
		return errors.New("routing.Batches: cannot start without registered routes")
	}
	ctx = context.WithoutCancel(ctx)

	err := context.Tasks(ctx).Once(
		ctx,
		"handleInput",
		func(ctx context.Context) error {
			b.handleInput(ctx)
			for _, r := range b.routes {
				close(r.out)
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("routing.Batches.Start: could not start routing: %w", err)
	}
	b.started = true

	return nil
}

// handleInput receives data on the input channel and pushes it to the appropriate receivers.
func (b *Router) handleInput(ctx context.Context) {
	for entry := range b.in {
		b.route(ctx, entry)
	}
}

// route sends entry to every registered route.
func (b *Router) route(ctx context.Context, entry data.Entry) {
	metrics.DataEntry(ctx, entry)
	for _, r := range b.routes {
		// Logging every drop would allocate on each one, which is when the router is already behind. Logging at each
		// power of two keeps the log bounded while still showing that drops continue and how many there have been.
		if n := b.push(r, entry); n != 0 && n&(n-1) == 0 {
			context.Log(ctx).Error(fmt.Sprintf("routing.Batches.push: dropped %d entries so far to slow receiver(%s)", n, r.name))
		}
	}
}

// push pushes a batches to a route. The router only stops when its input channel is closed, so a blocking push
// waits on the route for as long as it takes.
// It returns 0 if entry was delivered, or the number of entries dropped on the route so far if it was dropped.
func (b *Router) push(r route, entry data.Entry) (dropped uint64) {
	if b.blocking {
		r.out <- entry
		return 0
	}

	if !chans.TryPut(r.out, entry) {
		return r.dropped.Add(1)
	}
	return 0
}
