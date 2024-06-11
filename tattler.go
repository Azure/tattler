package tattler

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/tattler/batching"
	"github.com/Azure/tattler/data"
	preprocess "github.com/Azure/tattler/internal/preprocess"
	"github.com/Azure/tattler/internal/routing"
	"github.com/Azure/tattler/internal/safety"
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

	logger *slog.Logger

	mu      sync.Mutex
	started bool
}

// Option is an option for New().
type Option func(*Runner) error

// WithLogger sets the logger. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(r *Runner) error {
		if l == nil {
			return fmt.Errorf("logger cannot be nil")
		}
		r.logger = l
		return nil
	}
}

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

// New constructs a new Runner.
func New(ctx context.Context, in chan data.Entry, batchTimespan time.Duration, options ...Option) (*Runner, error) {
	if in == nil {
		return nil, fmt.Errorf("input channel cannot be nil")
	}

	r := &Runner{
		input:  in,
		logger: slog.Default(),
	}

	for _, o := range options {
		if err := o(r); err != nil {
			return nil, err
		}
	}
	r.batchOpts = append(r.batchOpts, batching.WithLogger(r.logger))

	if batchTimespan <= 0 {
		return nil, fmt.Errorf("batchTimespan must be greater than 0")
	}

	batchingIn := make(chan data.Entry, 1)
	routerIn := make(chan batching.Batches, 1)

	var secretsIn = in

	if r.preProcessors != nil {
		secretsIn = make(chan data.Entry, 1)
		_, err := preprocess.New(ctx, in, secretsIn, r.preProcessors, preprocess.WithLogger(r.logger))
		if err != nil {
			return nil, err
		}
	}

	secrets, err := safety.New(ctx, secretsIn, batchingIn, safety.WithLogger(r.logger))
	if err != nil {
		return nil, err
	}

	batcher, err := batching.New(ctx, batchingIn, routerIn, batchTimespan, r.batchOpts...)
	if err != nil {
		return nil, err
	}

	router, err := routing.New(ctx, routerIn, routing.WithLogger(r.logger))
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
		return fmt.Errorf("Reader(%T).SetOut(): %w", r, err)
	}
	if r.started {
		log.Println("here")
		if err := reader.Run(ctx); err != nil {
			log.Println("got here")
			return fmt.Errorf("reader(%T): %w", reader, err)
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
	return nil
}
