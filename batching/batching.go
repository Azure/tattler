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
	"errors"
	"iter"
	"log/slog"
	"time"

	"github.com/Azure/tattler/data"
	metrics "github.com/Azure/tattler/internal/metrics/batching"
	"github.com/gostdlib/base/concurrency/sync"
	"github.com/gostdlib/base/context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var (
	batchesPool = sync.NewPool(
		context.Background(),
		"batchesPool",
		func() Batches {
			return Batches{}
		},
	)
	batchPool = sync.NewPool(
		context.Background(),
		"batchPool",
		func() *Batch {
			b := Batch{Data: Data{}}
			return &b
		},
	)
)

var _ sync.Resetter = Batches{}
var _ sync.Resetter = &Batch{}

// Batches is a map of entry types to batches.
type Batches map[data.SourceType]*Batch

// Recycle returns Batches to a sync.Pool for reuse.
func (b Batches) Recycle() {
	batchesPool.Put(context.Background(), b)
}

// Reset implements sync.Restter.
func (b Batches) Reset() {
	for _, batch := range b {
		batchPool.Put(context.Background(), batch)
	}
	clear(b)
}

// All returns an iterator over data entries.
func (b Batches) All() iter.Seq[data.Entry] {
	return func(yield func(data.Entry) bool) {
		for _, batch := range b {
			for _, d := range batch.Data {
				if !yield(d) {
					return
				}
			}
		}
	}
}

// Len returns the length of the batches.
func (b Batches) Len() int {
	l := 0
	for _, batch := range b {
		l += len(batch.Data)
	}
	return l
}

// Batch stores batch data and metadata.
type Batch struct {
	Data Data
	age  time.Time
}

// Reset implements sync.Resetter.
func (b *Batch) Reset() {
	clear(b.Data)
	b.age = time.Time{}
}

// Map returns a map to batch data.
func (b *Batch) Map() map[types.UID]data.Entry {
	return b.Data.Map()
}

// Data is a map of UIDs to data.
type Data map[types.UID]data.Entry

// Map returns a map of UIDs to data entries.
func (b Data) Map() map[types.UID]data.Entry {
	if b == nil {
		return nil
	}
	return b
}

// Batcher is used to ingest data and emit batches.
type Batcher struct {
	timespan  time.Duration
	current   Batches
	batchSize int

	in  <-chan data.Entry
	out chan Batches

	emitter func(context.Context)

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
// is hit, it will emit regardless of the batch size. This defaults to 1000 items. Setting this
// to zero will make this only emit when the timespan is hit. However, this is rarely a good idea,
// as spikes can cause this to eat memory like crazy and cause serious CPU burn.
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
		timespan:  timespan,
		batchSize: 1000,
		in:        in,
		out:       out,
		log:       slog.Default(),
	}
	b.current = batchesPool.Get(ctx)
	b.emitter = b.emit

	for _, o := range options {
		if err := o(b); err != nil {
			return nil, err
		}
	}

	go b.run(ctx)

	return b, nil
}

// run runs the Batcher loop.
func (b *Batcher) run(ctx context.Context) {
	defer close(b.out)

	timer := time.NewTimer(b.timespan)
	ticker := time.NewTicker(b.timespan)
	defer ticker.Stop()

	for {
		timer.Reset(b.timespan)

		exit, err := b.handleInput(context.WithoutCancel(ctx), timer.C)
		if err != nil {
			b.log.Error(err.Error())
		}
		if exit {
			return
		}
	}
}

// handleInput handles the input data and batching when the ticker fires.
func (b *Batcher) handleInput(ctx context.Context, tick <-chan time.Time) (exit bool, err error) {
	select {
	case data, ok := <-b.in:
		if !ok {
			return true, nil
		}
		if err := b.handleData(ctx, data); err != nil {
			metrics.Error(ctx)
			return false, err
		}

		if b.batchSize > 0 {
			if b.current.Len() == b.batchSize {
				b.emitter(ctx)
			} else if b.current.Len() > b.batchSize {
				b.log.Error("Bug: batch size exceeded in Batcher")
				b.emitter(ctx)
			}
		}
	case <-tick:
		if b.current.Len() == 0 {
			metrics.Success(ctx)
			return false, nil
		}
		b.emitter(ctx)
	}
	metrics.Success(ctx)
	return false, nil
}

// emit emits the current batches and preps for the new batches. This is assigned
// to b.emitter by New() at runtime.
func (b *Batcher) emit(ctx context.Context) {
	batches := b.current
	for sourceType, batch := range batches {
		metrics.Emitted(ctx, sourceType, len(batch.Data), time.Since(batch.age))
	}

	n := batchesPool.Get(ctx)
	b.current = n
	b.out <- batches
}

type getMeta interface {
	GetCreationTimestamp() metav1.Time
}

// handleData handles putting the data into the current batch.
func (b *Batcher) handleData(ctx context.Context, entry data.Entry) error {
	batch, ok := b.current[entry.SourceType()]
	if !ok {
		batch = batchPool.Get(ctx)
		batch.Data = map[types.UID]data.Entry{}
		batch.age = time.Now()
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
