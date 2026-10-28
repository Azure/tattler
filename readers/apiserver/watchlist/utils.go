package watchlist

import (
	"fmt"
	"strconv"
	"time"

	"github.com/Azure/tattler/readers/apiserver/watchlist/internal/apierr"
	"github.com/Azure/tattler/readers/apiserver/watchlist/types"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/errors"
	"github.com/gostdlib/base/retry/exponential"
	"github.com/gostdlib/base/values/chans"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

// recoveryListLimit is the recovery list page size, the same as client-go's reflector uses.
const recoveryListLimit = 500

// bookmarkCheckTimeout is the time budget for one bookmarkAhead check as a whole: every list attempt and the backoff
// between them. A single List can be slow or throttled (client-go retries a 429 inside List, up to ten times by
// default), and the check runs under startWatch's 30s watchdog, so it is bounded in time, not only in attempts.
const bookmarkCheckTimeout = 5 * time.Second

// bookmarkCheckAttempts caps the list retries within bookmarkCheckTimeout. bookmarkCheckBack waits about 1s and then
// 2s (each up to 50% shorter or longer) between attempts, so they start at about 0s, 1s and 3s; a fourth would start
// at about 7s, after the budget. The timeout ends the check either way.
const bookmarkCheckAttempts = 3

// bookmarkAhead reports whether bookmark is newer than the server's current resourceVersion for sp. A resumed watch
// does not ask for initial events, and in that mode the APIServer never rejects a resourceVersion it has not reached
// yet: the watch opens and silently delivers nothing until storage catches up, which after an etcd restore or
// rollback is never. The current version comes from a consistent list of one object. The API treats
// resourceVersions as opaque, but the APIServer itself parses them as uint64
// (k8s.io/apiserver pkg/storage/api_object_versioner.go), so this does the same and reports an error, rather than
// a verdict, for a version that does not parse. The whole check, retries included, ends after timeout with an
// error; only ctx's child is cancelled by it, never ctx.
func bookmarkAhead(ctx context.Context, sp watchSpec, bookmark string, timeout time.Duration) (bool, error) {
	have, err := strconv.ParseUint(bookmark, 10, 64)
	if err != nil {
		return false, fmt.Errorf("bookmark %q is not a numeric resourceVersion: %w", bookmark, err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var current string
	err = bookmarkCheckBack.Retry(
		ctx,
		func(ctx context.Context, _ exponential.Record) error {
			obj, err := sp.list(ctx, metav1.ListOptions{Limit: 1})
			if err != nil {
				return apierr.Permanent(err)
			}
			list, err := meta.ListAccessor(obj)
			if err != nil {
				return fmt.Errorf("invalid list: %w: %w", err, exponential.ErrPermanent)
			}
			current = list.GetResourceVersion()
			return nil
		},
		exponential.WithMaxAttempts(bookmarkCheckAttempts),
	)
	if err != nil {
		return false, fmt.Errorf("error listing the current resourceVersion for %v: %w", sp.gvr, err)
	}

	want, err := strconv.ParseUint(current, 10, 64)
	if err != nil {
		return false, fmt.Errorf("server resourceVersion %q for %v is not numeric: %w", current, sp.gvr, err)
	}
	return have > want, nil
}

// errListRecovery is returned by the WatchList recovery request when connectWatcher has fallen back to standard
// watches, so recovery must list before it watches.
var errListRecovery = errors.New("WatchList is unavailable, recovery must list before watching")

// getRecoveryWatcher uses WatchList's initial-events boundary when available. Without WatchList, it captures
// a consistent paginated list and starts the watch at that list's version before delivering any snapshots.
func (r *Reader) getRecoveryWatcher(ctx context.Context, rt types.Retrieve, sp watchSpec) (watch.Interface, error) {
	w, err := r.getInitialEventsWatcher(ctx, rt, sp)
	if !errors.Is(err, errListRecovery) {
		return w, err
	}
	return r.getListRecoveryWatcher(ctx, rt, sp)
}

// getInitialEventsWatcher starts a WatchList recovery watch. It returns errListRecovery without starting a watch if
// the server does not support WatchList.
func (r *Reader) getInitialEventsWatcher(ctx context.Context, rt types.Retrieve, sp watchSpec) (watch.Interface, error) {
	return r.getWatcher(ctx, rt, func(options metav1.ListOptions) (watch.Interface, error) {
		if options.SendInitialEvents == nil {
			return nil, errListRecovery
		}
		w, err := sp.spawn(options)
		if err != nil {
			return nil, err
		}
		return newInitialEventsWatcher(w), nil
	})
}

// getListRecoveryWatcher lists on the caller's goroutine, so a large list does not hold the spawner that every other
// watcher needs, and only the watch from the list's version goes through the spawner.
func (r *Reader) getListRecoveryWatcher(ctx context.Context, rt types.Retrieve, sp watchSpec) (watch.Interface, error) {
	objects, resourceVersion, err := listRecoveryObjects(ctx, sp)
	if err != nil {
		return nil, err
	}
	w, err := r.getWatcher(ctx, rt, withResourceVersion(sp.spawn, resourceVersion))
	if err != nil {
		return nil, err
	}
	return newListRecoveryWatcher(w, objects, resourceVersion), nil
}

// listRecoveryObjects lists every object for sp and returns them with the list's resource version.
func listRecoveryObjects(ctx context.Context, sp watchSpec) ([]runtime.Object, string, error) {
	if sp.list == nil {
		return nil, "", fmt.Errorf("no recovery lister for %v: %w", sp.gvr, exponential.ErrPermanent)
	}

	var objects []runtime.Object
	var resourceVersion string
	listOptions := metav1.ListOptions{Limit: recoveryListLimit}
	for {
		obj, err := sp.list(ctx, listOptions)
		if err != nil {
			return nil, "", apierr.Permanent(err)
		}
		list, err := meta.ListAccessor(obj)
		if err != nil {
			return nil, "", fmt.Errorf("invalid recovery list: %w: %w", err, exponential.ErrPermanent)
		}
		if resourceVersion == "" {
			resourceVersion = list.GetResourceVersion()
		}
		if resourceVersion == "" || resourceVersion != list.GetResourceVersion() {
			return nil, "", fmt.Errorf("inconsistent recovery list resource version for %v: %w", sp.gvr, exponential.ErrPermanent)
		}
		page, err := meta.ExtractListWithAlloc(obj)
		if err != nil {
			return nil, "", fmt.Errorf("invalid recovery list items: %w: %w", err, exponential.ErrPermanent)
		}
		objects = append(objects, page...)
		listOptions.Continue = list.GetContinue()
		if listOptions.Continue == "" {
			return objects, resourceVersion, nil
		}
	}
}

// getWatcher sends a spawnWatcher function to the provided channel and waits for the resulting watch.Interface or an error.
func (r *Reader) getWatcher(ctx context.Context, rt types.Retrieve, sp spawnWatcher) (watch.Interface, error) {
	promise := spawnReqMaker.New(ctx, sp)
	if !chans.Put(ctx, r.spawnCh, promise) {
		return nil, context.Cause(ctx)
	}

	resp, err := promise.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("error creating %v watcher: %w", rt, err)
	}
	if resp.Err != nil {
		return nil, fmt.Errorf("error creating %v watcher: %w", rt, resp.Err)
	}

	return resp.V, nil
}
