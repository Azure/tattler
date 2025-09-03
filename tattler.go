/*
Package tattler provides a way to read date from a source called a Reader that provides K8 objects from
a K8 object source (such as etcd or API Server watchlist), preprocess the data to add or remove fields,
and then send the data to a Processor that will process the data in some way (such as sending it to a file, database
or service).

Reader types can be found in the /reader directory. Processor types are left for the user to implement.

See the README.md for more information on how to use this package.
*/
package tattler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/tattler/batching"
	"github.com/Azure/tattler/data"
	preprocess "github.com/Azure/tattler/internal/preprocess"
	"github.com/Azure/tattler/internal/routing"
	"github.com/Azure/tattler/internal/safety"
	"go.opentelemetry.io/otel/metric"

	batchingmetrics "github.com/Azure/tattler/internal/metrics/batching"
	readersmetrics "github.com/Azure/tattler/internal/metrics/readers"
)

// Reader defines the interface that must be implemented by all readers.
// We do not support a Reader that is not within this package.
type Reader interface {
	// SetOut sets the output channel that the reader must output on. Must return an error and be a no-op
	// if Run() has been called.
	SetOut(context.Context, chan data.Entry) error
	// Run starts the Reader processing. You may only call this once if Run() does not return an error.
	Run(context.Context) error
}

// PreProcessor is function that processes data before it is sent to a processor. It must be thread-safe.
// This is where you would alter data before it is sent for processing. Any change here affects
// all processors.
type PreProcessor = preprocess.Processor

// Runner runs readers and sends the output through a series data modifications and batching until
// it is sent to data processors.
type Runner struct {
	input         chan data.Entry
	secrets       *safety.Secrets
	batchOpts     []batching.Option
	batcher       *batching.Batcher
	router        *routing.Batches
	readers       []Reader
	preProcessors []PreProcessor

	meterProvider metric.MeterProvider

	mu      sync.Mutex
	started bool
}

// Option is an option for New().
type Option func(*Runner) error

// WithPreProcessor appends PreProcessors to the Runner.
func WithPreProcessor(p ...PreProcessor) Option {
	return func(r *Runner) error {
		r.preProcessors = append(r.preProcessors, p...)
		return nil
	}
}

// WithBatcherOptions sets the options for the Batcher.
func WithBatcherOptions(o ...batching.Option) Option {
	return func(r *Runner) error {
		r.batchOpts = append(r.batchOpts, o...)
		return nil
	}
}

// WithMeterProvider sets the meter provider with which to register metrics.
// Defaults to nil, in which case metrics won't be registered.
func WithMeterProvider(m metric.MeterProvider) Option {
	return func(r *Runner) error {
		if m == nil {
			return fmt.Errorf("meter cannot be nil")
		}
		r.meterProvider = m
		return nil
	}
}

// New constructs a new Runner. The input channel is the ouput of a Reader object. The batchTimespan
// is the duration to wait before sending a batch of data to the processor. There is also a maximum of
// 1000 entries that can be sent in a batch. This can be adjust by using WithBatcherOptions(batchingWithBatchSize()).
func New(ctx context.Context, in chan data.Entry, batchTimespan time.Duration, options ...Option) (*Runner, error) {
	if in == nil {
		return nil, fmt.Errorf("input channel cannot be nil")
	}

	r := &Runner{
		input: in,
	}

	for _, o := range options {
		if err := o(r); err != nil {
			return nil, err
		}
	}

	if batchTimespan <= 0 {
		return nil, fmt.Errorf("batchTimespan must be greater than 0")
	}

	batchingIn := make(chan data.Entry, 1)
	routerIn := make(chan batching.Batches, 1)

	var secretsIn = in

	if r.preProcessors != nil {
		secretsIn = make(chan data.Entry, 1)
		_, err := preprocess.New(ctx, in, secretsIn, r.preProcessors)
		if err != nil {
			return nil, err
		}
	}

	if r.meterProvider != nil {
		meter := r.meterProvider.Meter("tattler")
		if err := batchingmetrics.Init(meter); err != nil {
			return nil, err
		}
		if err := readersmetrics.Init(meter); err != nil {
			return nil, err
		}
	}

	secrets, err := safety.New(ctx, secretsIn, batchingIn)
	if err != nil {
		return nil, err
	}

	batcher, err := batching.New(ctx, batchingIn, routerIn, batchTimespan, r.batchOpts...)
	if err != nil {
		return nil, err
	}

	router, err := routing.New(ctx, routerIn)
	if err != nil {
		return nil, err
	}

	r.secrets = secrets
	r.batcher = batcher
	r.router = router

	return r, nil
}

// AddReader adds a reader's output channel as input to be processed. A Reader does not need to have
// SetOut() or Run() called, as these are handled by AddReader() and Start(). You can add a reader
// after Start() has been called. This allows staggering the start of readers.
func (r *Runner) AddReader(ctx context.Context, reader Reader) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := reader.SetOut(ctx, r.input); err != nil {
		return fmt.Errorf("Reader(%T).SetOut(): %w", reader, err)
	}
	if r.started {
		if err := reader.Run(ctx); err != nil {
			return fmt.Errorf("Reader(%T).Run(): %w", reader, err)
		}
	}
	r.readers = append(r.readers, reader)
	return nil
}

// AddProcessor registers a processors input to receive Batches data. This cannot be called
// after Start() has been called.
func (r *Runner) AddProcessor(ctx context.Context, name string, in chan batching.Batches) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if in == nil {
		return fmt.Errorf("in cannot be nil")
	}

	if r.started {
		return fmt.Errorf("cannot add a processor after Runner has started")
	}

	return r.router.Register(ctx, name, in)
}

// Start starts the Runner.
func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, reader := range r.readers {
		if err := reader.Run(ctx); err != nil {
			return fmt.Errorf("reader(%T): %w", reader, err)
		}
	}

	if err := r.router.Start(ctx); err != nil {
		return err
	}
	r.started = true
	return nil
}
