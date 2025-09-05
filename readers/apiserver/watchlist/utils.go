package watchlist

import (
	"fmt"
	"time"

	"github.com/Azure/tattler/readers/apiserver/watchlist/types"
	"github.com/gostdlib/base/context"
	"k8s.io/apimachinery/pkg/watch"
)

// getWatcher sends a spawnWatcher function to the provided channel and waits for the resulting watch.Interface or an error.
func (r *Reader) getWatcher(ctx context.Context, rt types.Retrieve, sp spawnWatcher) (watch.Interface, error) {
	promise := spawnReqMaker.New(ctx, sp)
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case r.spawnCh <- promise:
	}

	resp, err := promise.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("error creating %v watcher: %v", rt, err)
	}
	if resp.Err != nil {
		return nil, fmt.Errorf("error creating %v watcher: %v", rt, resp.Err)
	}

	return resp.V, nil
}

// resetTimer resets the timer t to d. If the timer has already fired, it will drain the channel.
func resetTimer(d time.Duration, t *time.Timer) {
	select {
	case <-t.C:
	default:
	}
	t.Reset(d)
}
