package watchlist

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/internal/filter/types/watchlist"
	filter "github.com/Azure/tattler/internal/filter/types/watchlist"
	metrics "github.com/Azure/tattler/internal/metrics/readers"
	"github.com/Azure/tattler/readers/apiserver/watchlist/relist"
	"github.com/Azure/tattler/readers/apiserver/watchlist/types"
	"github.com/gostdlib/base/concurrency/sync"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/retry/exponential"
	"github.com/gostdlib/base/values/generics/promises"
	"github.com/gostdlib/base/values/generics/sets"

	"go.opentelemetry.io/otel/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8Types "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

var back = exponential.Must(exponential.New())

// lister defines the interface for relisting resources
type lister interface {
	List(ctx context.Context, rt types.Retrieve) (chan promises.Response[data.Entry], error)
}

// Reader reports changes made to data objects on the APIServer via the watchlist API.
type Reader struct {
	cancel context.CancelFunc

	clientset     kubernetes.Interface
	retrieveTypes types.Retrieve
	filter        *watchlist.Filter
	filterIn      chan watch.Event
	filterOpts    []watchlist.Option
	// relistInterval indicates how often we should relist all the objects in the APIServer.
	// The default is never. The minimum is 1 hour and the maximum is 7 days.
	// This can only be set via a Constructor option.
	relistInterval time.Duration
	relister       lister

	spawnCh           chan promises.Promise[spawnWatcher, watch.Interface]
	watcherSpawnDelay time.Duration

	dataCh        chan data.Entry
	waitWatchers  sync.Group
	cancelWatches context.CancelFunc
	started       bool
	meterProvider metric.MeterProvider

	// List functions for each resource type
	listFunctions map[types.Retrieve][]spawnLister

	closeCh chan struct{}
	mu      sync.Mutex

	// For testing.
	fakeWatch              func(context.Context, types.Retrieve, []spawnWatcher) error
	fakeWatchEvents        func(context.Context, watch.Interface) (string, error)
	testHandleClientSwitch func()
	testPerformRelist      func()
}

// Option is an option for New(). Unused for now.
type Option func(*Reader) error

// WithFilterSize sets the initial size of the filter map.
func WithFilterSize(size int) Option {
	return func(c *Reader) error {
		c.filterOpts = append(c.filterOpts, watchlist.WithSized(size))
		return nil
	}
}

// WithRelist will set a duration in which we will relist all the objects in the APIServer using List() API calls.
// This is useful to prevent split brain scenarios where the APIServer and tattler have
// different views of the world. All objects returned by List() are marked as snapshots.
// The default is never. The minimum is 1 hour and the maximum is 7 days.
func WithRelist(d time.Duration) Option {
	return func(c *Reader) error {
		if d < 1*time.Hour {
			return errors.New("relist duration must be at least 1 hour")
		}
		if d > 7*24*time.Hour {
			return errors.New("relist duration must be less than 7 days")
		}
		c.relistInterval = d
		return nil
	}
}

// rtMap is a dynamically created map of RetrieveType.
var rtMap = map[types.Retrieve]func(ctx context.Context) []spawnWatcher{}
var rtMapOnce sync.Once

func init() {
	var i uint32 = 0

	for {
		rt := types.Retrieve(1 << i)
		if strings.HasPrefix(rt.String(), "Retrieve(") {
			break
		}
		rtMap[rt] = nil
		i++
	}
}

// New creates a new Reader object. retrieveTypes is a bitwise flag to determine what data to retrieve.
func New(ctx context.Context, clientset kubernetes.Interface, retrieveTypes types.Retrieve, opts ...Option) (*Reader, error) {
	if clientset == nil {
		return nil, fmt.Errorf("clientset is nil")
	}

	relister, err := relist.New(clientset)
	if err != nil {
		return nil, fmt.Errorf("error creating relister: %v", err)
	}

	ctx, cancel := context.WithCancel(ctx)

	r := &Reader{
		clientset:         clientset,
		retrieveTypes:     retrieveTypes,
		relistInterval:    -1, // This indicates never.
		relister:          relister,
		cancel:            cancel,
		closeCh:           make(chan struct{}),
		spawnCh:           make(chan promises.Promise[spawnWatcher, watch.Interface]),
		watcherSpawnDelay: 10 * time.Second,
	}

	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, err
		}
	}

	// Make sure they passed a valid retrieveTypes.
	found := false
	for rt := range rtMap {
		if retrieveTypes&rt == rt {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("no data types to retrieve")
	}

	_ = context.Tasks(ctx).Once(
		ctx,
		"watchlistSpawnWatchers",
		func(ctx context.Context) error {
			r.connectWatcher(ctx, r.spawnCh)
			return nil
		},
	)

	return r, nil
}

// Close closes the Reader. This will stop all watchers, but does not close the output channel.
func (r *Reader) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.close(ctx)
}

func (r *Reader) close(ctx context.Context) error {
	if !r.started {
		return fmt.Errorf("cannot call Close before Run is called")
	}

	// TODO(jdoak): This has gotten messy with a combination of closeCh and context cancel.
	// Make this a context cancel only and remove Close()/close().
	close(r.closeCh)
	r.cancel()
	r.cancelWatches()

	_ = r.waitWatchers.Wait(ctx)
	close(r.filterIn)
	close(r.spawnCh)
	return nil
}

// SetOut sets the output channel for data to flow out on.
func (r *Reader) SetOut(ctx context.Context, out chan data.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.started {
		return fmt.Errorf("cannot call SetOut once the Reader has had Start() called")
	}
	r.dataCh = out
	return nil
}

// spawnWatcher is a function that creates a watcher for a resource.
type spawnWatcher func(options metav1.ListOptions) (watch.Interface, error)

// spawnLister is a function that creates a lister for a resource.
type spawnLister func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error)

// Run starts the Reader. This will start all watchers and begin sending data to the output channel.
func (r *Reader) Run(ctx context.Context) (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.started {
		return fmt.Errorf("cannot call Run once the Reader has already started")
	}
	if r.dataCh == nil {
		return fmt.Errorf("cannot call Run if SetOut has not been called(%v)", r.dataCh)
	}

	defer func() {
		if err != nil {
			r.close(ctx)
		}
	}()

	if err := r.setupFilter(ctx); err != nil {
		return fmt.Errorf("error setting up cache: %v", err)
	}

	ctx, r.cancelWatches = context.WithCancel(context.WithoutCancel(ctx))

	for rt := range rtMap {
		if r.retrieveTypes&rt == rt {
			if err := r.startWatch(ctx, r.cancelWatches, rt); err != nil {
				if errors.Is(err, context.Canceled) {
					err = fmt.Errorf("could not connect to server by deadline")
				}
				return fmt.Errorf("error starting %s watcher: %v", rt.String(), err)
			}
		}
	}

	r.relistTask(ctx)
	r.started = true
	return nil
}

// startWatch starts a watcher for a resource type. This will return an error if the watcher could not
// be started. If the context is canceled, this will return an error.
// This is a funky thing because if the cluster is not working, a call to Watch() will hang indefinitely.
// If you pass a context to Watch(), if it connects it will error when the context is canceled.
// So, we have to watch for a bit in another goroutine and then cancel the context if it doesn't connect.
func (r *Reader) startWatch(ctx context.Context, cancel context.CancelFunc, rt types.Retrieve) error {
	finished := make(chan struct{})
	timer := time.After(30 * time.Second)

	go func() {
		select {
		case <-finished:
		case <-timer:
			cancel()
		}
	}()

	var spanWatchers []spawnWatcher
	switch rt {
	case types.RTNamespace:
		spanWatchers = r.createNamespaceWatcher(ctx)
	case types.RTNode:
		spanWatchers = r.createNodesWatcher(ctx)
	case types.RTPod:
		spanWatchers = r.createPodsWatcher(ctx)
	case types.RTPersistentVolume:
		spanWatchers = r.createPersistentVolumesWatcher(ctx)
	case types.RTRBAC:
		spanWatchers = r.createRBACWatcher(ctx)
	case types.RTService:
		spanWatchers = r.createServicesWatcher(ctx)
	case types.RTDeployment:
		spanWatchers = r.createDeploymentsWatcher(ctx)
	case types.RTIngressController:
		spanWatchers = r.createIngressesWatcher(ctx)
	case types.RTEndpoint:
		spanWatchers = r.createEndpointsWatcher(ctx)
	default:
		return fmt.Errorf("unknown resource type: %v", rt)
	}

	if err := r.watch(ctx, rt, spanWatchers); err != nil {
		return fmt.Errorf("error starting %s watcher: %v", rt, err)
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
	r.filter, err = filter.New(ctx, r.filterIn, r.dataCh, r.filterOpts...)
	if err != nil {
		return fmt.Errorf("error creating cache: %v", err)
	}
	return nil
}

var spawnReqMaker = &promises.Maker[spawnWatcher, watch.Interface]{}

// watch watches a set of resources and sends the events to the cache. If this returns
// an error, the initial watcher could not be created. This will return nil
// if the initial watcher is created, but underlying calls are in a goroutine.
// This will handle automatic reconnection.
func (r *Reader) watch(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) (err error) {
	if r.fakeWatch != nil {
		return r.fakeWatch(ctx, rt, spawnWatchers)
	}

	if ctx.Err() != nil {
		return nil
	}

	watchers := make([]watch.Interface, len(spawnWatchers))
	defer func() {
		if err != nil {
			for _, watcher := range watchers {
				if watcher != nil {
					watcher.Stop()
				}
			}
		}
	}()

	for i, sp := range spawnWatchers {
		w, err := r.getWatcher(ctx, rt, sp)
		if err != nil {
			return err
		}
		watchers[i] = w

		r.waitWatchers.Go(
			ctx,
			func(ctx context.Context) error {
				return r.handleWatcher(ctx, rt, w, sp)
			},
		)
	}

	return nil
}

// handleWatcher handles a single watcher. It takes the watcher and hands it off to watchEvents. If watchEvents returns
// an error and Context is not cancelled, our retrier will try and get a new watcher. This will block until the context
// is canceled.
func (r *Reader) handleWatcher(ctx context.Context, rt types.Retrieve, w watch.Interface, sp spawnWatcher) error {
	for {
		// This blocks until watchEvents returns, which is when a watcher is closed.
		var err error
		_, err = r.watchEvents(ctx, w)
		if err != nil {
			context.Log(ctx).Error(fmt.Sprintf("error watching %v events: %v", rt, err))
		}
		// This would indicate that the watcher was intentionally closed.
		if ctx.Err() != nil {
			return nil
		}

		err = back.Retry(
			ctx,
			func(ctx context.Context, _ exponential.Record) error {
				var err error
				w, err = r.getWatcher(ctx, rt, sp)
				if err != nil {
					context.Log(ctx).Error(fmt.Sprintf("error re-creating watcher(%v): %v", rt, err))
				}
				return err
			},
		)
		if err != nil {
			context.Log(ctx).Error(fmt.Sprintf("critical error on watcher(%v) recreation: %v", rt, err))
			return err
		}
	}
	panic("unreachable")
}

var spawnOptions = metav1.ListOptions{
	ResourceVersion: "",
	Watch:           true,
}

// connectWatcher takes a request to create a watcher and creates it, sending the result back on the promise.
// This prevents thundering herds of watchers trying to connect at the same time.
func (r *Reader) connectWatcher(ctx context.Context, ch chan promises.Promise[spawnWatcher, watch.Interface]) {
	for req := range ch {
		watcher, err := req.In(spawnOptions)
		_ = req.Set(ctx, watcher, err) // Error only happens on context cancelation

		// We have no way in which we can wait for a watcher to finish its initial pool of events because we
		// have no idea what that is (like a List()). So, we try to put a pause on it to give time for initial processing
		// before we try to create another watcher.
		time.Sleep(r.watcherSpawnDelay)
	}
}

// watchEvents watches the events from a watcher and sends them to the cache.
// This is subbed in for .watchEvents in the Reader struct.
// If the watcher can emit bookmarks, the string returned will be the resource version.
// This blocks until the watcher is closed.
func (r *Reader) watchEvents(ctx context.Context, watcher watch.Interface) (string, error) {
	if r.fakeWatchEvents != nil {
		return r.fakeWatchEvents(ctx, watcher)
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
		metrics.WatchEvent(ctx, event)
		switch event.Type {
		case watch.Bookmark:
			return event.Object.(metav1.Object).GetResourceVersion(), nil
		case watch.Error:
			context.Log(ctx).Error(fmt.Sprintf("Watch Error: %v", event.Object))
			return "", nil
		}
		r.filterIn <- event
	}

	return "", nil
}

// relistTask will spin off a goroutine that waits for the timer to expire or the close channel to be closed.
// If the close channel closes, this function will return.
// If the timer expires, this function will perform a relist operation and reset the timer.
func (r *Reader) relistTask(ctx context.Context) {
	if r.testHandleClientSwitch != nil {
		r.testHandleClientSwitch()
		return
	}

	if r.relistInterval <= 0 {
		return
	}

	t := time.NewTimer(r.relistInterval)

	context.Tasks(ctx).Once(
		ctx,
		"reslitTask",
		func(context.Context) error {
			for {
				select {
				case <-r.closeCh:
					return nil
				case <-t.C:
					if r.testPerformRelist != nil {
						r.testPerformRelist()
					}

					if err := r.performRelist(ctx); err != nil {
						context.Log(ctx).Error(fmt.Sprintf("error performing relist: %v", err))
					}
					t.Reset(r.relistInterval)
				}
			}
		},
	)
}

// performRelist performs a relist operation by calling List() for all active resource types. This handles all
// exponential backoff and retries. If the context is canceled, this will return an error.
func (r *Reader) performRelist(ctx context.Context) error {
	for rt := range rtMap {
		if r.retrieveTypes&rt == rt {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ch chan promises.Response[data.Entry]

			err := back.Retry(
				ctx,
				func(ctx context.Context, _ exponential.Record) error {
					seen := sets.Set[k8Types.UID]{}

					if ctx.Err() != nil {
						return ctx.Err()
					}
					var err error
					ch, err = r.relister.List(ctx, rt)
					if err != nil {
						context.Log(ctx).Error(fmt.Sprintf("relister.List had supported type error: %s", err.Error()))
						return err
					}
					for entry := range ch {
						if entry.Err != nil {
							return err
						}
						if seen.Contains(entry.V.UID()) {
							continue
						}
						seen.Add(entry.V.UID())
						select {
						case <-ctx.Done():
							return fmt.Errorf("relist context canceled: %w", ctx.Err())
						case r.dataCh <- entry.V:
						}
					}
					return err
				},
				exponential.WithMaxAttempts(7),
			)
			if err != nil {
				context.Log(ctx).Error(fmt.Sprintf("relister.List(%T) had persistent errors: %s", rt, err.Error()))
			}
		}
	}
	return nil
}
