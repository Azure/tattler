package watchlist

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/Azure/tattler/data"
	metrics "github.com/Azure/tattler/internal/metrics/readers"
	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store"
	"github.com/Azure/tattler/readers/apiserver/watchlist/relist"
	"github.com/Azure/tattler/readers/apiserver/watchlist/types"
	"github.com/gostdlib/base/concurrency/sync"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/errors"
	"github.com/gostdlib/base/retry/exponential"
	"github.com/gostdlib/base/values/chans"
	"github.com/gostdlib/base/values/generics/promises"
	"github.com/gostdlib/base/values/generics/sets"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8Types "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
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
	// relistInterval indicates how often we should relist all the objects in the APIServer.
	// The default is never. The minimum is 1 hour and the maximum is 7 days.
	// This can only be set via a Constructor option.
	relistInterval time.Duration
	relister       lister

	spawnCh           chan promises.Value[spawnWatcher, watch.Interface]
	watcherSpawnDelay time.Duration
	bookmarking       bool
	bookmarkStore     store.Bookmarks

	dataCh        chan data.Entry
	waitWatchers  sync.Group
	cancelWatches context.CancelFunc
	started       bool

	closeCh chan struct{}
	mu      sync.Mutex

	// For testing.
	fakeWatch              func(context.Context, types.Retrieve, []watchSpec) error
	fakeWatchEvents        func(context.Context, watch.Interface) (string, error)
	testHandleClientSwitch func()
	testPerformRelist      func()
}

// Option for New().
type Option func(*Reader) error

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

// WithBookmarkStore sets a store used to load and persist watch bookmark resourceVersions.
// On Kubernetes versions before 1.29, the Reader continues without bookmark persistence.
func WithBookmarkStore(store store.Bookmarks) Option {
	return func(c *Reader) error {
		if store == nil {
			return errors.New("bookmark store is nil")
		}
		c.bookmarkStore = store
		return nil
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

	ver, err := majorMinor(clientset)
	if err != nil {
		return nil, err
	}

	// Make sure they passed a valid retrieveTypes: at least one flag, and no bit that is not a known flag.
	if retrieveTypes == 0 {
		return nil, fmt.Errorf("no data types to retrieve")
	}
	for i := 0; i < 32; i++ {
		bit := types.Retrieve(1 << i)
		if retrieveTypes&bit != 0 && !bit.Valid() {
			return nil, fmt.Errorf("unknown data type to retrieve: %v", bit)
		}
	}

	r := &Reader{
		clientset:         clientset,
		retrieveTypes:     retrieveTypes,
		relistInterval:    -1, // This indicates never.
		relister:          relister,
		closeCh:           make(chan struct{}),
		spawnCh:           make(chan promises.Value[spawnWatcher, watch.Interface]),
		watcherSpawnDelay: 10 * time.Second,
		waitWatchers:      context.Pool(ctx).Default().Group(),
	}

	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, err
		}
	}

	if ver[0] != 1 || ver[1] >= 29 {
		r.bookmarking = true
	}
	if r.bookmarkStore != nil && !r.bookmarking {
		context.Log(ctx).Warn(
			"bookmark persistence disabled because Kubernetes does not support watch lists",
			"kubernetesVersion", fmt.Sprintf("%d.%d", ver[0], ver[1]),
		)
	}

	// The cancellable context is created only once nothing else can fail, so an error return cannot leak it.
	ctx, r.cancel = context.WithCancel(ctx)

	if err = context.Tasks(ctx).Once(
		ctx,
		"watchlistSpawnWatchers",
		func(ctx context.Context) error {
			r.connectWatcher(ctx, r.spawnCh)
			return nil
		},
	); err != nil {
		r.cancel()
		return nil, fmt.Errorf("error starting the watcher spawner: %w", err)
	}

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

	defer close(r.spawnCh)
	if err := r.waitWatchers.Wait(ctx); err != nil {
		return fmt.Errorf("watchers stopped with errors: %w", err)
	}
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

type watchSpec struct {
	gvr   schema.GroupVersionResource
	spawn spawnWatcher
}

// withResourceVersion returns sp with the watch resuming from resourceVersion. A resumed WatchList watch does not
// ask for initial events, as those objects were already sent before the bookmark was taken. If resourceVersion is
// empty, sp is returned unchanged so the watch starts with the current state of every object.
func withResourceVersion(sp spawnWatcher, resourceVersion string) spawnWatcher {
	if resourceVersion == "" {
		return sp
	}
	return func(options metav1.ListOptions) (watch.Interface, error) {
		options.ResourceVersion = resourceVersion
		// nil means the server does not support WatchList and connectWatcher fell back to a standard watch.
		if options.SendInitialEvents != nil {
			options.SendInitialEvents = ptr.To(false)
		}
		return sp(options)
	}
}

// withoutInitialEvents returns sp with a WatchList watch starting at the most recent resourceVersion without sending
// existing objects. It is used after a rejected bookmark, where relistGap() sends the current state as a snapshot
// instead. A standard watch (nil SendInitialEvents, used when the server does not support WatchList) cannot start at
// the most recent resourceVersion without one, so it still replays existing objects.
func withoutInitialEvents(sp spawnWatcher) spawnWatcher {
	return func(options metav1.ListOptions) (watch.Interface, error) {
		if options.SendInitialEvents != nil {
			options.SendInitialEvents = ptr.To(false)
		}
		return sp(options)
	}
}

func bookmarkName(gvr schema.GroupVersionResource) string {
	if gvr.Empty() {
		return ""
	}
	group := gvr.Group
	if group == "" {
		// Core Kubernetes resources use an empty API group in GroupVersionResource.
		// .v1.pods vs core.v1.pods
		group = "core"
	}
	return fmt.Sprintf("%s.%s.%s", group, gvr.Version, gvr.Resource)
}

func (r *Reader) storeBookmark(ctx context.Context, key schema.GroupVersionResource, resourceVersion string) error {
	if r.bookmarkStore == nil {
		return nil
	}
	if err := r.bookmarkStore.Store(ctx, key, resourceVersion); err != nil {
		context.Log(ctx).Error(fmt.Sprintf("error storing bookmark(%s): %v", bookmarkName(key), err))
		return err
	}
	return nil
}

// clearBookmark deletes the stored bookmark for key if it is still resourceVersion. If another Reader sharing the store
// has already replaced it, the replacement is left alone but not resumed from: this Reader did not see the events
// before it, so it recovers with relistGap() instead.
func (r *Reader) clearBookmark(ctx context.Context, key schema.GroupVersionResource, resourceVersion string) error {
	if r.bookmarkStore == nil || key.Empty() {
		return nil
	}
	err := r.bookmarkStore.Delete(ctx, key, resourceVersion)
	if err == nil || errors.Is(err, store.ErrBookmarkChanged) {
		return nil
	}
	return fmt.Errorf("error clearing bookmark(%s): %w", bookmarkName(key), err)
}

// relistGap sends a snapshot of rt after one of its bookmarks was rejected, so consumers can reconcile the changes,
// including deletes, that happened while the bookmark was stale. The watcher must already be running so nothing after
// the list is missed. This runs in waitWatchers so Close() waits for it.
func (r *Reader) relistGap(ctx context.Context, rt types.Retrieve) {
	context.Log(ctx).Warn(fmt.Sprintf("bookmark for %v was rejected, relisting to recover changes missed while it was stale", rt))
	// The Group gets a context that cannot be cancelled, because Group records a cancelled context as an error and
	// Close() would report the cancellation it caused.
	r.waitWatchers.Go(
		context.WithoutCancel(ctx),
		func(context.Context) error {
			if ctx.Err() != nil {
				return nil
			}
			r.relistType(ctx, rt)
			return nil
		},
	)
}

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

	// Watchers outlive Run, so they must not stop when the caller's ctx does. watchCtx drops the caller's cancellation
	// and is cancelled only by the Reader: by Close(), by the startup watchdog, or by a failed Run below.
	watchCtx, cancelWatches := context.WithCancel(context.WithoutCancel(ctx))
	r.cancelWatches = cancelWatches

	// If any type fails to start, stop the watchers already started for other types. This does not use close(): the
	// Reader has not started, so Close() must keep refusing, and closeCh and spawnCh stay open so Run can be retried.
	defer func() {
		if err != nil {
			r.cancelWatches()
			_ = r.waitWatchers.Wait(ctx)
		}
	}()

	for rt := range types.ListRetrieve() {
		if r.retrieveTypes&rt == rt {
			if err = r.startWatch(watchCtx, r.cancelWatches, rt); err != nil {
				return err
			}
		}
	}

	r.relistTask(watchCtx)
	r.started = true
	return nil
}

// startWatch starts a watcher for a resource type. This will return an error if the watcher could not
// be started. If the context is canceled, this will return an error.
// This is a funky thing because if the cluster is not working, a call to Watch() will hang indefinitely.
// If you pass a context to Watch(), if it connects it will error when the context is canceled.
// So, we cancel the context if the watchers have not connected within a deadline.
func (r *Reader) startWatch(ctx context.Context, cancel context.CancelFunc, rt types.Retrieve) error {
	deadline := 30 * time.Second

	var ws []watchSpec
	switch rt {
	case types.RTNamespace:
		ws = r.createNamespaceWatcher(ctx)
	case types.RTNode:
		ws = r.createNodesWatcher(ctx)
	case types.RTPod:
		ws = r.createPodsWatcher(ctx)
	case types.RTPersistentVolume:
		ws = r.createPersistentVolumesWatcher(ctx)
	case types.RTRBAC:
		deadline = 2 * time.Minute // This one spawns 4 watchers
		ws = r.createRBACWatcher(ctx)
	case types.RTService:
		ws = r.createServicesWatcher(ctx)
	case types.RTDeployment:
		ws = r.createDeploymentsWatcher(ctx)
	case types.RTIngressController:
		ws = r.createIngressesWatcher(ctx)
	case types.RTEndpoint:
		ws = r.createEndpointsWatcher(ctx)
	default:
		return fmt.Errorf("unknown resource type: %v", rt)
	}

	// Stopping the watchdog when watch() returns, success or failure, means it can never fire later and cancel
	// watchers that have nothing to do with this attempt. Run() handles cleanup when this returns an error.
	watchdog := time.AfterFunc(deadline, cancel)
	defer watchdog.Stop()

	err := r.watch(ctx, rt, ws)
	// Run() detaches ctx from its caller, so the only thing that can cancel it here is the watchdog.
	switch {
	case ctx.Err() != nil && err != nil:
		return fmt.Errorf("error starting %v watcher: could not connect to server within %v: %w: %w", rt, deadline, ctx.Err(), err)
	case ctx.Err() != nil:
		return fmt.Errorf("error starting %v watcher: could not connect to server within %v: %w", rt, deadline, ctx.Err())
	case err != nil:
		return fmt.Errorf("error starting %v watcher: %w", rt, err)
	}
	return nil
}

var spawnReqMaker = &promises.Maker[spawnWatcher, watch.Interface]{}

// watch watches a set of resources and sends the events to the cache. If this returns
// an error, the initial watcher could not be created. This will return nil
// if the initial watcher is created, but underlying calls are in a goroutine.
// This will handle automatic reconnection.
func (r *Reader) watch(ctx context.Context, rt types.Retrieve, ws []watchSpec) (err error) {
	if r.fakeWatch != nil {
		return r.fakeWatch(ctx, rt, ws)
	}

	if ctx.Err() != nil {
		return nil
	}

	// gap is set if any stored bookmark was rejected, which means changes were missed and rt must be relisted.
	gap := false
	watchers := make([]watch.Interface, len(ws))
	for i, rw := range ws {
		key := rw.gvr
		sp := rw.spawn
		var resourceVersion string
		if r.bookmarking && r.bookmarkStore != nil && !key.Empty() {
			var err error
			resourceVersion, err = r.bookmarkStore.Load(ctx, key)
			if err != nil {
				context.Log(ctx).Error(fmt.Sprintf("error loading bookmark: %v", err))
				resourceVersion = ""
			}
		}
		w, err := r.getWatcher(ctx, rt, withResourceVersion(sp, resourceVersion))
		if err != nil && resourceVersion != "" && rejectedResourceVersion(err) {
			if clearErr := r.clearBookmark(ctx, key, resourceVersion); clearErr != nil {
				return clearErr
			}
			resourceVersion = ""
			gap = true
			w, err = r.getWatcher(ctx, rt, withoutInitialEvents(sp))
		}
		if err != nil {
			return err
		}
		watchers[i] = w

		// sync.Group skips a function whose context is already cancelled when its goroutine starts, which would leak w
		// if Close() or a failed Run() cancels right after this. The Group gets a context that cannot be cancelled so
		// w is always stopped, and handleWatcher gets ctx so it still exits as soon as ctx is cancelled.
		r.waitWatchers.Go(
			context.WithoutCancel(ctx),
			func(context.Context) error {
				defer w.Stop()
				return r.handleWatcher(ctx, rt, key, resourceVersion, w, sp)
			},
		)
	}

	if gap {
		r.relistGap(ctx, rt)
	}
	return nil
}

// handleWatcher handles a single watcher. It takes the watcher and hands it off to watchEvents. If watchEvents returns
// an error and Context is not cancelled, our retrier will try and get a new watcher. This will block until the context
// is canceled.
func (r *Reader) handleWatcher(ctx context.Context, rt types.Retrieve, key schema.GroupVersionResource, resourceVersion string, w watch.Interface, sp spawnWatcher) error {
	// gap is set when a bookmark was rejected while reconnecting, which means changes were missed and rt must be relisted.
	gap := false
	for {
		rv, err := r.watchEvents(ctx, key, w)
		if rv != "" {
			resourceVersion = rv
		}
		if err != nil {
			context.Log(ctx).Error(fmt.Sprintf("error watching %v events: %v", rt, err))
		}
		if ctx.Err() != nil {
			return nil
		}

		err = back.Retry(
			ctx,
			func(ctx context.Context, _ exponential.Record) error {
				attemptedResourceVersion := resourceVersion != ""
				wrapped := sp
				switch {
				case r.bookmarking && attemptedResourceVersion:
					wrapped = withResourceVersion(sp, resourceVersion)
				case gap:
					wrapped = withoutInitialEvents(sp)
				}
				var err error
				w, err = r.getWatcher(ctx, rt, wrapped)
				if err != nil {
					switch {
					case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
						return fmt.Errorf("%w: %w", err, exponential.ErrPermanent)
					case attemptedResourceVersion && rejectedResourceVersion(err):
						if clearErr := r.clearBookmark(ctx, key, resourceVersion); clearErr != nil {
							return clearErr
						}
						resourceVersion = ""
						gap = true
					case !attemptedResourceVersion && invalidWatchRequest(err):
						return fmt.Errorf("%w: %w", err, exponential.ErrPermanent)
					}
					context.Log(ctx).Error(fmt.Sprintf("error re-creating watcher(%v): %v", rt, err))
				}
				return err
			},
		)
		switch {
		// Retry gives up with the context's error when the Reader is closed, which is a normal stop, not a failure.
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			context.Log(ctx).Error(fmt.Sprintf("critical error on watcher(%v) recreation: %v", rt, err))
			return fmt.Errorf("error re-creating %v watcher: %w", rt, err)
		}
		if gap {
			r.relistGap(ctx, rt)
			gap = false
		}
	}
}

// connectWatcher takes a request to create a watcher and creates it, sending the result back on the promise.
// This prevents thundering herds of watchers trying to connect at the same time.
func (r *Reader) connectWatcher(ctx context.Context, ch chan promises.Value[spawnWatcher, watch.Interface]) {
	watchListEnabled := r.bookmarking
	so := metav1.ListOptions{Watch: true}
	if r.bookmarking {
		// A watch without a resourceVersion streams every existing object first, the same as a standard watch does.
		// When resuming from a bookmark, withResourceVersion() sets the resourceVersion and turns initial events off
		// so reconnects do not replay all watched objects.
		so.AllowWatchBookmarks = true
		so.ResourceVersionMatch = metav1.ResourceVersionMatchNotOlderThan
		so.SendInitialEvents = ptr.To(true)
	}

	for req := range ch {
		watcher, err := req.In(so)
		if watchListFeatureDisabled(err) && r.bookmarking && watchListEnabled {
			context.Log(ctx).Warn("WatchList feature is unavailable; falling back to a standard watch")
			watchListEnabled = false
			so.ResourceVersionMatch = ""
			so.SendInitialEvents = nil
			watcher, err = req.In(so)
		}
		req.Set(ctx, watcher, err) // Error only happens on context cancelation

		// We have no way in which we can wait for a watcher to finish its initial pool of events because we
		// have no idea what that is (like a List()). So, we try to put a pause on it to give time for initial processing
		// before we try to create another watcher.
		time.Sleep(r.watcherSpawnDelay)
	}
}

// watchListFeatureDisabled reports whether err is the API server rejecting sendInitialEvents because the WatchList
// feature gate is off. The API server (k8s.io/apimachinery v0.34.0-beta.0) returns this as a generic Invalid status
// whose only distinguishing detail is the cause message, so there is no typed error or reason code to check.
func watchListFeatureDisabled(err error) bool {
	if !apierrors.IsInvalid(err) {
		return false
	}
	var statusErr apierrors.APIStatus
	if !errors.As(err, &statusErr) || statusErr.Status().Details == nil {
		return false
	}
	for _, cause := range statusErr.Status().Details.Causes {
		if cause.Field == "sendInitialEvents" && strings.Contains(cause.Message, "WatchList feature gate") {
			return true
		}
	}
	return false
}

func staleResourceVersion(err error) bool {
	return apierrors.IsResourceExpired(err) || apierrors.IsGone(err)
}

func invalidWatchRequest(err error) bool {
	return apierrors.IsBadRequest(err) || apierrors.IsInvalid(err)
}

// rejectedResourceVersion reports whether the APIServer rejected the supplied resource version as stale or invalid.
func rejectedResourceVersion(err error) bool {
	return staleResourceVersion(err) || invalidWatchRequest(err)
}

// watchEvents watches the events from a watcher and sends them to the cache.
// This is subbed in for .watchEvents in the Reader struct.
// If the watcher can emit bookmarks, the string returned will be the resource version.
// This blocks until the watcher is closed.
func (r *Reader) watchEvents(ctx context.Context, key schema.GroupVersionResource, watcher watch.Interface) (string, error) {
	if r.fakeWatchEvents != nil {
		return r.fakeWatchEvents(ctx, watcher)
	}

	ch := watcher.ResultChan()

	var resourceVersion string
	var storedResourceVersion string

	stopper := sync.OnceFunc(
		func() {
			watcher.Stop()
		},
	)

	for {
		rv, err := r.watchEvent(ctx, ch, stopper)
		if rv != "" {
			resourceVersion = rv
			if rv != storedResourceVersion && r.storeBookmark(ctx, key, rv) == nil {
				storedResourceVersion = rv
			}
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
	// Get keeps returning buffered events after ctx is done, so anything but ResultOK stops the watcher. Close()
	// cancels ctx and then waits for every watcher to exit.
	event, res := chans.Get(ctx, ch)
	if res != chans.ResultOK {
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
	entry, err := data.WatchEventToEntry(event)
	if err != nil {
		context.Log(ctx).Error(fmt.Sprintf("error converting watch event to entry: %v", err))
		return "", nil
	}
	// A stalled consumer must not keep this watcher alive after Close(), which cancels ctx and then waits for
	// every watcher to exit.
	if !chans.Put(ctx, r.dataCh, entry) {
		stopper()
		return "", io.EOF
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
	for rt := range types.ListRetrieve() {
		if r.retrieveTypes&rt == rt {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.relistType(ctx, rt)
		}
	}
	return nil
}

// relistType lists every object of rt and sends each one as a snapshot entry. This handles all exponential backoff
// and retries. Errors are logged, not returned.
func (r *Reader) relistType(ctx context.Context, rt types.Retrieve) {
	var ch chan promises.Response[data.Entry]
	// seen spans every attempt so that a retry after a partial list does not resend what was already sent.
	seen := sets.Set[k8Types.UID]{}

	err := back.Retry(
		ctx,
		func(ctx context.Context, _ exponential.Record) error {
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
					// relist.Relister sends the error as its last value and then closes ch, so returning
					// here leaves nothing blocked on the channel.
					return entry.Err
				}
				if seen.Contains(entry.V.UID()) {
					continue
				}
				seen.Add(entry.V.UID())
				if !chans.Put(ctx, r.dataCh, entry.V) {
					return fmt.Errorf("relist context canceled: %w", ctx.Err())
				}
			}
			// The relister can drop its error once ctx is done, so a closed channel is only a complete list if ctx is not.
			if ctx.Err() != nil {
				return fmt.Errorf("relist context canceled: %w", ctx.Err())
			}
			return nil
		},
		exponential.WithMaxAttempts(7),
	)
	if err != nil {
		context.Log(ctx).Error(fmt.Sprintf("relister.List(%v) had persistent errors: %s", rt, err.Error()))
	}
}

// majorMinor extracts the major and minor version of the K8 cluster.
func majorMinor(clientset kubernetes.Interface) ([2]int, error) {
	nfo, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return [2]int{}, fmt.Errorf("can't determine k8 version: %w", err)
	}

	return [2]int{convertToDecimal(nfo.Major), convertToDecimal(nfo.Minor)}, nil
}

/*
convertToDecimal tries to deal with the fact that K8 hates you.
In K8, we love being stringly typed.  Instead of having concrete representations and a struct that means something,
we get strings for each part (major/minor) along with other nonsense. So you can get things like this (along with whatever
other nonsense people come up with):
  - Major = "1"
  - Minor = "28"
  - Minor = "27+"
  - Minor = "29-alpha.1"
  - Minor = "30-rc.0"

Therefore, we have to try to abstract some semblance of sanity from this. This is a more forgiving strconv.Atoi() that handles
that minor can have crazy things after it. We use it for major too as it saves us a split on ".".
*/
func convertToDecimal(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsDigit(r) {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}
