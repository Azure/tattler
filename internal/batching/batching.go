/*
Package batching provides batching operations for reader data that removes any older data for the same item
that is sent during the batch time.

The batch is not size based, as we don't actually have a way to determine the batch size because
we haven't encoded into bytes. To control sizing, we can adjust the amount of time we wait or by simple item
count.

The Batcher will emit a Batches map of data types to a Batch map. The Batch is a map of UIDs to data. We
overwrite any old data that with new data that comes in with the same UID, a different ResourceVersion and an older
creation date . This allows us to get rid of older data before we emit the batch.

Usage is pretty simple:

	batcher, err := batching.New(ctx, 5 * time.Second, WithBatchSize(1000))
	if err != nil {
		// Do something
	}

	// Handle the output.
	go func() {
		for _, batches := range batcher.Out() {
			for data := range batches.Iter() {
				// Do something with data
			}
			// Then recycle the batch, if your sure you're done with it.
			batches.Recycle()
		}
	}()

	// Send input to the batcher.
	for entry := range r.Stream() { // where r is a some reader returning data.Entry
		batcher.In() <- entry
	}
*/
package batching

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/tattler/data"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var (
	batchesPool = sync.Pool{
		New: func() any {
			return &Batches{}
		},
	}
	batchPool = sync.Pool{
		New: func() any {
			return &Batch{}
		},
	}
)

func getBatches() Batches {
	return *batchesPool.Get().(*Batches)
}

func getBatch() Batch {
	return *batchPool.Get().(*Batch)
}

// Batches is a map of entry types to batches.
type Batches map[data.SourceType]Batch

// Recyle recycles the batches. It should not be used after this.
func (b Batches) Recyle() {
	for batchesK, batch := range b {
		for batchK := range batch {
			delete(batch, batchK)
		}
		batchPool.Put(&batch)
		delete(b, batchesK)
	}
	batchPool.Put(&b)
}

// Iter returns a channel that iterates over the data. Closing ctx will stop the iteration.
func (b Batches) Iter(ctx context.Context) <-chan data.Entry {
	ch := make(chan data.Entry, 1)
	go func() {
		defer close(ch)
		for _, batch := range b {
			for _, d := range batch {
				select {
				case <-ctx.Done():
					return
				case ch <- d:
				}
			}
		}
	}()
	return ch
}

// Batch is a map of UIDs to data.
type Batch map[types.UID]data.Entry

func (b *Batch) Map() map[types.UID]data.Entry {
	if b == nil {
		return nil
	}
	return *b
}

// Batcher is used to ingest data and emit batches.
type Batcher struct {
	timespan  time.Duration
	current   Batches
	batchSize int

	in  <-chan data.Entry
	out chan Batches

	emitter func()

	log *slog.Logger
}

// Option is a opional argument for New().
type Option func(*Batcher) error

// WithLogger sets the logger.
func WithLogger(log *slog.Logger) Option {
	return func(b *Batcher) error {
		b.log = log
		return nil
	}
}

// WithBatchSize sets the batch size at which to emit at. So if you set this to 1000, it will
// emit when it has 1000 items in the batch if we haven't hit the timespan. If the timespan
// is hit, it will emit regardless of the batch size.
func WithBatchSize(size int) Option {
	return func(b *Batcher) error {
		if size < 0 {
			return errors.New("batch size must be greater than 0")
		}
		b.batchSize = size
		return nil
	}
}

// New creates a new Batcher.
func New(ctx context.Context, in <-chan data.Entry, out chan Batches, timespan time.Duration, options ...Option) (*Batcher, error) {
	if in == nil || out == nil {
		return nil, errors.New("can't call Batcher.New() with a nil in or out channel")
	}

	b := &Batcher{
		timespan: timespan,
		in:       in,
		out:      out,
		log:      slog.Default(),
	}
	b.current = getBatches()
	b.emitter = b.emit

	for _, o := range options {
		if err := o(b); err != nil {
			return nil, err
		}
	}

	go b.run()

	return b, nil
}

// run runs the Batcher loop.
func (b *Batcher) run() {
	defer close(b.out)

	timer := time.NewTimer(b.timespan)
	ticker := time.NewTicker(b.timespan)
	defer ticker.Stop()

	for {
		timer.Reset(b.timespan)
		exit, err := b.handleInput(timer.C)
		if err != nil {
			b.log.Error(err.Error())
		}
		if exit {
			return
		}
	}
}

// handleInput handles the input data and batching when the ticker fires.
func (b *Batcher) handleInput(tick <-chan time.Time) (exit bool, err error) {
	select {
	case data, ok := <-b.in:
		if !ok {
			return true, nil
		}
		if err := b.handleData(data); err != nil {
			return false, err
		}
		if b.batchSize > 0 && len(b.current) == b.batchSize {
			b.emitter()
		}
	case <-tick:
		if len(b.current) == 0 {
			return false, nil
		}
		b.emitter()
	}
	return false, nil
}

// emit emits the current batches and preps for the new batches. This is assigned
// to b.emitter by New() at runtime.
func (b *Batcher) emit() {
	batches := b.current
	n := getBatches()
	b.current = n
	b.out <- batches
}

type getMeta interface {
	GetCreationTimestamp() metav1.Time
}

// handleData handles putting the data into the current batch.
func (b *Batcher) handleData(entry data.Entry) error {
	batch, ok := b.current[entry.SourceType()]
	if !ok {
		batch = getBatch()
	}

	if entry.UID() == "" {
		return errors.New("no UID for entry")
	}

	if entry.ChangeType() == data.CTDelete {
		batch.Map()[entry.UID()] = entry
		b.current[entry.SourceType()] = batch
		return nil
	}
	old, ok := batch.Map()[entry.UID()]
	if !ok {
		batch.Map()[entry.UID()] = entry
		b.current[entry.SourceType()] = batch
		return nil
	}
	ots := old.Object().(getMeta).GetCreationTimestamp().Time
	nts := entry.Object().(getMeta).GetCreationTimestamp().Time
	if nts.After(ots) {
		batch.Map()[entry.UID()] = entry
		b.current[entry.SourceType()] = batch
	}
	return nil
}
