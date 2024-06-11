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
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Azure/tattler/internal/batching"

	"github.com/gostdlib/concurrency/prim/wait"
)

type route struct {
	out  chan batching.Batches
	name string
}

type routes []route

// Batches routes batches to registered destinations.
type Batches struct {
	input   chan batching.Batches
	routes  routes
	started bool

	log *slog.Logger
}

// Option is an optional argument to New().
type Option func(b *Batches) error

// WithLogger sets Batches to use a custom Logger.
func WithLogger(l *slog.Logger) Option {
	return func(b *Batches) error {
		if l == nil {
			return fmt.Errorf("WithLogger does not accept a nil *slog.Logger")
		}
		b.log = l
		return nil
	}
}

// New is the constructor for Batches.
func New(ctx context.Context, input chan batching.Batches, options ...Option) (*Batches, error) {
	if input == nil {
		return nil, errors.New("routing.New: input channel cannot be nil")
	}

	b := &Batches{
		input:  input,
		routes: routes{},
		log:    slog.Default(),
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
func (b *Batches) Register(ctx context.Context, name string, ch chan batching.Batches) error {
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
func (b *Batches) Exists(name string) bool {
	for _, r := range b.routes {
		if r.name == name {
			return true
		}
	}
	return false
}

// Start starts routing data coming from input. This can be stopped by closing the input channel.
func (b *Batches) Start(ctx context.Context) error {
	if len(b.routes) == 0 {
		return errors.New("routing.Batches: cannot start without registered routes")
	}
	ctx = context.WithoutCancel(ctx)
	b.started = true

	g := wait.Group{}
	g.Go(ctx, func(ctx context.Context) error {
		b.handleInput(ctx)
		return nil
	})

	go func() {
		g.Wait(ctx)
		for _, r := range b.routes {
			close(r.out)
		}
	}()

	return nil
}

// handleInput receives data on the input channel and pushes it to the appropriate receivers.
func (b *Batches) handleInput(ctx context.Context) {
	for batches := range b.input {
		for _, r := range b.routes {
			if err := b.push(ctx, r, batches); err != nil {
				b.log.Error(err.Error())
			}
		}
	}
}

// push pushes a batches to a route.
func (b *Batches) push(ctx context.Context, r route, batches batching.Batches) error {
	select {
	case r.out <- batches:
	default:
		return fmt.Errorf("routing.Batches.handleInformer: dropping data to slow receiver(%s)", r.name)
	}
	return nil
}
