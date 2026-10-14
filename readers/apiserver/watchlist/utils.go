package watchlist

import (
	"fmt"

	"github.com/Azure/tattler/readers/apiserver/watchlist/types"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/values/chans"
	"k8s.io/apimachinery/pkg/watch"
)

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
