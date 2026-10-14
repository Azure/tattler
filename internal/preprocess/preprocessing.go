/*
Package preprocessing provides preprocessing operations for reader data that alters the data before
it is sent to be processed. This allows for data to be altered safely without concurrency issues.
Processors are read only and are not allowed to alter data.
*/
package preprocess

import (
	"fmt"

	"github.com/Azure/tattler/data"
	"github.com/gostdlib/base/context"
)

// Processor is function that processes data before it is sent downstream. It must be thread-safe.
// Any change here affects everything downstream.
type Processor func(context.Context, data.Entry) error

// Runner runs a series of PreProcessors.
type Runner struct {
	in, out chan data.Entry
	procs   []Processor
}

// New creates a new Runner. A runner can be stopped by closing the input channel.
func New(ctx context.Context, in, out chan data.Entry, procs []Processor) (*Runner, error) {
	r := &Runner{
		in:    in,
		out:   out,
		procs: procs,
	}

	// run parks on in for the life of the pipeline, so it goes on the unlimited Default pool rather than taking a
	// slot from a Limited one.
	if !context.Pool(ctx).Default().Submit(ctx, func() { r.run(ctx) }) {
		return nil, fmt.Errorf("preprocess.New: could not start the runner: %w", context.Cause(ctx))
	}

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
