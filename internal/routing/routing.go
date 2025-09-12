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
	"errors"
	"fmt"

	"github.com/Azure/tattler/data"
	"github.com/gostdlib/base/context"
)

type route struct {
	out  chan data.Entry
	name string
}

type routes []route

// Router routes batches to registered destinations.
type Router struct {
	in       chan data.Entry
	routes   routes
	started  bool
	blocking bool
}

// Option is an optional argument to New().
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

	b.routes = append(b.routes, route{name: name, out: ch})
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
	b.started = true

	context.Tasks(ctx).Once(
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

	return nil
}

// handleInput receives data on the input channel and pushes it to the appropriate receivers.
func (b *Router) handleInput(ctx context.Context) {
	for entry := range b.in {
		for _, r := range b.routes {
			if err := b.push(ctx, r, entry); err != nil {
				context.Log(ctx).Error(err.Error())
			}
		}
	}
}

// push pushes a batches to a route.
func (b *Router) push(ctx context.Context, r route, entry data.Entry) error {
	if b.blocking {
		select {
		case r.out <- entry:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("routing.Batches.push: context canceled while pushing to %s: %w", r.name, context.Cause(ctx))
		}
	}

	select {
	case r.out <- entry:
	default:
		return fmt.Errorf("routing.Batches.push: dropping data to slow receiver(%s)", r.name)
	}
	return nil
}
