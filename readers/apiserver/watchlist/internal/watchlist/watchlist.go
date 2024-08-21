package watchlist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/internal/filter/types/watchlist"
	metrics "github.com/Azure/tattler/metrics/watchlist"
	filter "github.com/Azure/tattler/internal/filter/types/watchlist"
	"github.com/gostdlib/concurrency/prim/wait"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// Reader reports changes made to data objects on the APIServer via the watchlist API.
type Reader struct {
	clientset     *kubernetes.Clientset
	retrieveTypes RetrieveType
	filter        *watchlist.Filter
	filterIn      chan watch.Event
	filterOpts    []watchlist.Option
	// relist indicates how often we should relist all the objects in the APIServer.
	// The default is never. The minimum is 1 hour and the maximum is 7 days.
	// This can only be set via a Constructor option.
	relist time.Duration

	ch            chan data.Entry
	waitWatchers  wait.Group
	cancelWatches context.CancelFunc
	started       bool
	log           *slog.Logger

	// For testing.
	fakeWatch       func(context.Context, RetrieveType, spanWatcher) error
	fakeWatchEvents func(context.Context, watch.Interface) (string, error)
}

// Option is an option for New(). Unused for now.
type Option func(*Reader) error

// WithLogger sets the logger for the Changes object.
func WithLogger(log *slog.Logger) Option {
	return func(c *Reader) error {
		c.log = log
		return nil
	}
}

// WithFilterSize sets the initial size of the filter map.
func WithFilterSize(size int) Option {
	return func(c *Reader) error {
		c.filterOpts = append(c.filterOpts, watchlist.WithSized(size))
		return nil
	}
}

// WithRelist will set a duration in which we will relist all the objects in the APIServer.
// This is useful to prevent split brain scenarios where the APIServer and tattler have
// different views of the world. The default is never. The minimum is 1 hour and the maximum is 7 days.
func WithRelist(d time.Duration) Option {
	return func(c *Reader) error {
		if d < 1*time.Hour {
			return errors.New("relist duration must be at least 1 hour")
		}
		if d > 7*24*time.Hour {
			return errors.New("relist duration must be less than 7 days")
		}
		c.relist = d
		return nil
	}
}

//go:generate stringer -type=RetrieveType -linecomment

// RetrieveType is the type of data to retrieve. Uses as a bitwise flag.
// So, like: RTNode | RTPod, or RTNode, or RTPod.
type RetrieveType uint32

const (
	// RTNode retrieves node data.
	RTNode RetrieveType = 0x1 // Node
	// RTPod retrieves pod data.
	RTPod RetrieveType = 0x2 // Pod
	// RTNamespace retrieves namespace data.
	RTNamespace RetrieveType = 0x4 // Namespace
	// RTPersistentVolume retrieves persistent volume data.
	RTPersistentVolume RetrieveType = 0x8 // PersistentVolume
)

// New creates a new Reader object. retrieveTypes is a bitwise flag to determine what data to retrieve.
func New(ctx context.Context, clientset *kubernetes.Clientset, retrieveTypes RetrieveType, opts ...Option) (*Reader, error) {
	if clientset == nil {
		return nil, fmt.Errorf("clientset is nil")
	}

	r := &Reader{
		clientset:     clientset,
		log:           slog.Default(),
		retrieveTypes: retrieveTypes,
		relist:        -1, // This indicates never.
	}

	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, err
		}
	}
	r.filterOpts = append(r.filterOpts, watchlist.WithLogger(r.log))

	if retrieveTypes&RTNode != RTNode &&
		retrieveTypes&RTPod != RTPod &&
		retrieveTypes&RTNamespace != RTNamespace &&
		retrieveTypes&RTPersistentVolume != RTPersistentVolume {
		return nil, fmt.Errorf("no data types to retrieve")
	}

	return r, nil
}

// Logger returns the logger for the Reader.
func (r *Reader) Logger() *slog.Logger {
	return r.log
}

// Relist returns the relist duration for the Reader.
func (r *Reader) Relist() time.Duration {
	return r.relist
}

// Close closes the Reader. This will stop all watchers, but does not close the output channel.
func (r *Reader) Close(ctx context.Context) error {
	if !r.started {
		return fmt.Errorf("cannot call Close before Run is called")
	}

	r.cancelWatches()

	r.waitWatchers.Wait(ctx)
	close(r.filterIn)
	return nil
}

// SetOut sets the output channel for data to flow out on.
func (r *Reader) SetOut(ctx context.Context, out chan data.Entry) error {
	if r.started {
		return fmt.Errorf("cannot call SetOut once the Reader has had Start() called")
	}
	r.ch = out
	return nil
}

// spanWatcher is a function that creates a watcher for a resource.
type spanWatcher func(options metav1.ListOptions) (watch.Interface, error)

// Run starts the Reader. This will start all watchers and begin sending data to the output channel.
func (r *Reader) Run(ctx context.Context) (err error) {
	if r.started {
		return fmt.Errorf("cannot call Run once the Reader has already started")
	}
	if r.ch == nil {
		return fmt.Errorf("cannot call Run if SetOut has not been called(%v)", r.ch)
	}

	defer func() {
		if err != nil {
			r.Close(ctx)
		}
	}()

	if err := r.setupFilter(ctx); err != nil {
		return fmt.Errorf("error setting up cache: %v", err)
	}

	ctx, r.cancelWatches = context.WithCancel(context.WithoutCancel(ctx))

	if r.retrieveTypes&RTNamespace == RTNamespace {
		if err := r.startWatch(ctx, r.cancelWatches, RTNamespace); err != nil {
			if errors.Is(err, context.Canceled) {
				err = fmt.Errorf("could not connect to server by deadline")
			}
			return fmt.Errorf("error starting namespace watcher: %v", err)
		}
	}

	if r.retrieveTypes&RTPersistentVolume == RTPersistentVolume {
		if err := r.startWatch(ctx, r.cancelWatches, RTPersistentVolume); err != nil {
			if errors.Is(err, context.Canceled) {
				err = fmt.Errorf("could not connect to server by deadline")
			}
			return fmt.Errorf("error starting persistent volume watcher: %v", err)
		}
	}

	if r.retrieveTypes&RTNode == RTNode {
		if err := r.startWatch(ctx, r.cancelWatches, RTNode); err != nil {
			if errors.Is(err, context.Canceled) {
				err = fmt.Errorf("could not connect to server by deadline")
			}
			return fmt.Errorf("error starting node watcher: %v", err)
		}
	}

	if r.retrieveTypes&RTPod == RTPod {
		if err := r.startWatch(ctx, r.cancelWatches, RTPod); err != nil {
			if errors.Is(err, context.Canceled) {
				err = fmt.Errorf("could not connect to server by deadline")
			}
			return fmt.Errorf("error starting pod watcher: %v", err)
		}
	}

	return nil
}

// startWatch starts a watcher for a resource type. This will return an error if the watcher could not
// be started. If the context is canceled, this will return an error.
// This is a funky thing because if the cluster is not working, a call to Watch() will hang indefinitely.
// If you pass a context to Watch(), if it connects it will error when the context is canceled.
// So, we have to watch for a bit in another goroutine and then cancel the context if it doesn't connect.
func (r *Reader) startWatch(ctx context.Context, cancel context.CancelFunc, rt RetrieveType) error {
	finished := make(chan struct{})
	timer := time.After(30 * time.Second)

	go func() {
		select {
		case <-finished:
		case <-timer:
			cancel()
		}
	}()
	ws := func(options metav1.ListOptions) (watch.Interface, error) {
		switch rt {
		case RTNamespace:
			return r.clientset.CoreV1().Namespaces().Watch(ctx, options)
		case RTNode:
			return r.clientset.CoreV1().Nodes().Watch(ctx, options)
		case RTPod:
			return r.clientset.CoreV1().Pods("").Watch(ctx, options)
		case RTPersistentVolume:
			return r.clientset.CoreV1().PersistentVolumes().Watch(ctx, options)
		default:
			return nil, fmt.Errorf("unknown object type: %v", rt)
		}
	}

	if err := r.watch(ctx, rt, ws); err != nil {
		return fmt.Errorf("error starting namespace watcher: %v", err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("error starting namespace watcher: could not connect to server in time")
	}
	close(finished)
	return ctx.Err()
}

// setupFilter sets up the filter cache for the Reader.
func (r *Reader) setupFilter(ctx context.Context) error {
	r.filterIn = make(chan watch.Event, 1)

	var err error
	r.filter, err = filter.New(ctx, r.filterIn, r.ch, r.filterOpts...)
	if err != nil {
		return fmt.Errorf("error creating cache: %v", err)
	}
	return nil
}

// watch watches a resource and sends the events to the cache. If this returns
// an error, the initial watcher could not be created. This will return nil
// if the initial watcher is created, but underlying calls go on in a goroutine.
// This will handle automatic reconnection.
func (r *Reader) watch(ctx context.Context, rt RetrieveType, spanWatcher spanWatcher) error {
	if r.fakeWatch != nil {
		return r.fakeWatch(ctx, rt, spanWatcher)
	}

	if ctx.Err() != nil {
		return nil
	}

	var rv string
	options := metav1.ListOptions{
		ResourceVersion: rv,
		Watch:           true,
	}

	watcher, err := spanWatcher(options)
	if err != nil {
		return fmt.Errorf("error creating %v watcher: %v", rt, err)
	}

	r.waitWatchers.Go(ctx, func(ctx context.Context) error {
		for {
			var err error
			rv, err = r.watchEvents(ctx, watcher)
			if err != nil {
				r.log.Error(fmt.Sprintf("error watching %v events: %v", rt, err))
			}
			if ctx.Err() != nil {
				return nil
			}
			watcher, err = spanWatcher(options)
			if err != nil {
				r.log.Error(fmt.Sprintf("error creating %v watcher: %v", rt, err))
			}
		}
	})

	return nil
}

// watchEvents watches the events from a watcher and sends them to the cache.
// This is subbed in for .watchEvents in the Reader struct.
// If the watcher can emit bookmarks, the string returned will be the resource version.
// This blocks until the watcher is closed.
func (r *Reader) watchEvents(ctx context.Context, watcher watch.Interface) (string, error) {
	if r.fakeWatchEvents != nil {
		return r.watchEvents(ctx, watcher)
	}

	ch := watcher.ResultChan()

	var resourceVersion string

	stopper := sync.OnceFunc(
		func() {
			watcher.Stop()
		},
	)

	for {
		rv, err := r.watchEvent(ctx, ch, stopper)
		if rv != "" {
			resourceVersion = rv
		}
		// Always an io.EOF, so we don't log it, we just exit.
		if err != nil {
			break
		}
	}

	return resourceVersion, nil
}

// watchEvent watches a single event from a watcher and sends it to the cache. If the event
// is a bookmark, the resource version is returned. If the context is canceled or the input channel
// is closed, io.EOF is returned. If it is a watch.Error, the error is logged and nil is returned.
func (r *Reader) watchEvent(ctx context.Context, ch <-chan watch.Event, stopper func()) (string, error) {
	select {
	case <-ctx.Done():
		stopper()
		return "", io.EOF
	case event, ok := <-ch:
		if !ok {
			stopper()
			return "", io.EOF
		}
		switch event.Type {
		case watch.Bookmark:
			return event.Object.(metav1.Object).GetResourceVersion(), nil
		case watch.Error:
			r.log.Error(fmt.Sprintf("Watch Error: %v", event.Object))
			return "", nil
		}
		r.filterIn <- event
		metrics.RecordWatchEvent(ctx, event, time.Second)
	}

	return "", nil
}
