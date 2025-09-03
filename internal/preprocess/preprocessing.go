/*
Package preprocessing provides preprocessing operations for reader data that alters the data before
it is sent to be processed. This allows for data to be altered safely without concurrency issues.
Processors are read only and are not allowed to alter data.
*/
package preprocess

import (
	"context"

	"github.com/Azure/tattler/data"
)

// Processor is function that processes data before it is sent downstream. It must be thread-safe.
// Any change here affects everything downstream.
type Processor func(context.Context, data.Entry) error

// Runner runs a series of PreProcessors.
type Runner struct {
	in, out chan data.Entry
	procs   []Processor
}

// Option is an option for New().
type Option func(*Runner) error

// New creates a new Runner. A runner can be stopped by closing the input channel.
func New(ctx context.Context, in, out chan data.Entry, procs []Processor, options ...Option) (*Runner, error) {
	r := &Runner{
		in:    in,
		out:   out,
		procs: procs,
	}

	for _, o := range options {
		if err := o(r); err != nil {
			return nil, err
		}
	}

	go r.run(ctx)

	return r, nil
}

// run starts the Runner.
func (r *Runner) run(ctx context.Context) error {
	defer close(r.out)
	for entry := range r.in {
		if err := r.processEntry(ctx, entry); err != nil {
			continue
		}
		r.out <- entry
	}
	return nil
}

func (r *Runner) processEntry(ctx context.Context, entry data.Entry) error {
	for _, p := range r.procs {
		if err := p(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}
