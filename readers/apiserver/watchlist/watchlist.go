/*
Package watchlist provides a reader for the APIServer watchlist API.

The watchlist API provides all request objects on the APIServer and streams changes to those objects
as they are made. This is signficantly more efficient that the informer API, which using a bad
caching implementation to cache all the objects.

In addition, this provides a reslist option that can be used to time out the current reader and
create a new on underneath to refresh the view of the APIServer in case any updates were missed.
*/
package watchlist

// Note:  this package sits on top of the real reader package, but handles creating and deleting
// those reader objects whenever we need to refresh the full view of the APIServer.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/retry/exponential"
	"github.com/Azure/tattler/data"
	reader "github.com/Azure/tattler/readers/apiserver/watchlist/internal/watchlist"

	"go.opentelemetry.io/otel/metric"
	"k8s.io/client-go/kubernetes"
)

type watchReader interface {
	Logger() *slog.Logger
	Close(ctx context.Context) error
	SetOut(ctx context.Context, out chan data.Entry) error
	Run(ctx context.Context) (err error)
	Relist() time.Duration
}

// Reader reports changes made to data objects on the APIServer via the watchlist API.
type Reader struct {
	mu sync.Mutex // guards r
	r  watchReader
	// ch is the output channel for data to flow out on. It is set by .SetOut().
	ch chan data.Entry

	// newReader is a function that creates a new reader object using the
	// same configuration as the original reader.
	newReader func() (watchReader, error)

	logger        *slog.Logger
	meterProvider metric.MeterProvider

	closeCh chan struct{}

	testHandleClientSwitch func()
	testClientSwitchRetry  func()
}

// Option is an option for New(). Unused for now.
type Option = reader.Option

// WithLogger sets the logger for the Changes object.
func WithLogger(log *slog.Logger) Option {
	return reader.WithLogger(log)
}

// WithFilterSize sets the initial size of the filter map.
func WithFilterSize(size int) Option {
	return reader.WithFilterSize(size)
}

// WithRelist will set a duration in which we will relist all the objects in the APIServer.
// This is useful to prevent split brain scenarios where the APIServer and tattler have
// different views of the world. The default is never. The minimum is 1 hour and the maximum is 7 days.
func WithRelist(d time.Duration) Option {
	return reader.WithRelist(d)
}

// WithMeterProvider sets the meter provider with which to register metrics.
// Defaults to nil, in which case metrics won't be registered.
// You will not need to initialize this if already initialized in tattler Runner.
func WithMeterProvider(m metric.MeterProvider) Option {
	return reader.WithMeterProvider(m)
}

// RetrieveType is the type of data to retrieve. Uses as a bitwise flag.
// So, like: RTNode | RTPod, or RTNode, or RTPod.
type RetrieveType = reader.RetrieveType

const (
	// RTNode retrieves node data.
	RTNode = reader.RTNode
	// RTPod retrieves pod data.
	RTPod = reader.RTPod
	// RTNamespace retrieves namespace data.
	RTNamespace = reader.RTNamespace
	// RTPersistentVolume retrieves persistent volume data.
	RTPersistentVolume = reader.RTPersistentVolume
)

// New creates a new Reader object. retrieveTypes is a bitwise flag to determine what data to retrieve.
func New(ctx context.Context, clientset *kubernetes.Clientset, retrieveTypes RetrieveType, opts ...Option) (*Reader, error) {
	r, err := reader.New(ctx, clientset, retrieveTypes, opts...)
	if err != nil {
		return nil, err
	}

	newReader := func() (watchReader, error) {
		return reader.New(ctx, clientset, retrieveTypes, opts...)
	}

	return &Reader{
		r:         r,
		closeCh:   make(chan struct{}),
		newReader: newReader,
		logger:    r.Logger(),
	}, nil
}

// Close closes the Reader. This will stop all watchers and close the output channel.
func (r *Reader) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	close(r.closeCh)
	defer func() {
		if r.ch != nil {
			close(r.ch)
		}
	}()

	return r.r.Close(ctx)
}

// SetOut sets the output channel for data to flow out on.
func (r *Reader) SetOut(ctx context.Context, out chan data.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.r.SetOut(ctx, out); err != nil {
		return err
	}
	r.ch = out
	return nil
}

// Run starts the Reader. This will start all watchers and begin sending data to the output channel.
func (r *Reader) Run(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.r.Run(ctx); err != nil {
		return err
	}
	r.handleClientSwitch()
	return nil
}

// handleClientSwitch will handle the client switch logic if the relist option is set.
// This will create a new reader object and close the old one.
// If the Relist is <= 0, this function will return immediately.
func (r *Reader) handleClientSwitch() {
	if r.testHandleClientSwitch != nil {
		r.testHandleClientSwitch()
		return
	}

	go func() {
		if r.r.Relist() <= 0 {
			return
		}

		t := time.NewTimer(r.r.Relist())
		defer t.Stop()

		for {
			if exit := r.switchWait(t); exit {
				return
			}
		}
	}()
}

// switchWait will wait for the timer to expire or the close channel to close.
// If the close channel closes, this function will return true.
// If the timer expires, this function will switch the client and reset the timer.
func (r *Reader) switchWait(t *time.Timer) (exit bool) {
	select {
	case <-r.closeCh:
		return true
	case <-t.C:
		r.clientSwitchRetry()
		resetTimer(r.r.Relist(), t)
		return false
	}
}

// clientSwitchRetry will create a new reader object and close the old one. If there is
// an error creating the new reader, it will retry until it is successful or Close() is called.
func (r *Reader) clientSwitchRetry() {
	if r.testClientSwitchRetry != nil {
		r.testClientSwitchRetry()
		return
	}

	boff, _ := exponential.New() // nolint:errcheck // Can't error on default
	boff.Retry(                  // nolint:errcheck // We don't care about the error here.
		context.Background(),
		r.clientSwitch,
	)
}

// clientSwitch will create a new reader object and close the old one.
func (r *Reader) clientSwitch(ctx context.Context, rec exponential.Record) error {
	select {
	case <-r.closeCh:
		return nil
	default:
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	newReader, err := r.newReader()
	if err != nil {
		return err
	}

	if err := newReader.SetOut(context.Background(), r.ch); err != nil {
		return fmt.Errorf("bug: could not call SetOut() on new watchlist reader: %w", err)
	}
	if err := newReader.Run(context.Background()); err != nil {
		return err
	}
	if err := r.r.Close(context.Background()); err != nil {
		r.logger.Warn(fmt.Sprintf("could not close old watchlist reader: %v", err))
	}
	r.r = newReader
	return nil
}

// resetTimer resets the timer t to d. If the timer has already fired, it will drain the channel.
func resetTimer(d time.Duration, t *time.Timer) {
	select {
	case <-t.C:
	default:
	}
	t.Reset(d)
}
