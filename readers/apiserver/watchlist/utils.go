package watchlist

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/retry/exponential"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// retryableList wraps List() calls with exponential backoff using existing package boff.
func retryableList(ctx context.Context, lister func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error), options metav1.ListOptions, logger *slog.Logger) (runtime.Object, error) {
	var result runtime.Object
	err := back.Retry(ctx, func(ctx context.Context, rec exponential.Record) error {
		var retryErr error
		result, retryErr = lister(ctx, options)
		return retryErr // Let exponential backoff handle the retry logic
	}, exponential.WithMaxAttempts(10))

	if err != nil {
		logger.Error(fmt.Sprintf("List() failed after all retries: %v", err))
	}

	return result, err
}

// resetTimer resets the timer t to d. If the timer has already fired, it will drain the channel.
func resetTimer(d time.Duration, t *time.Timer) {
	select {
	case <-t.C:
	default:
	}
	t.Reset(d)
}
