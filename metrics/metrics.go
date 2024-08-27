package metrics

import (
	api "go.opentelemetry.io/otel/metric"

	"github.com/Azure/tattler/metrics/batching"
	"github.com/Azure/tattler/metrics/watchlist"
)


// Init initializes all tattler metrics.
func Init(meter api.Meter) error {
	if err := batching.Init(meter); err != nil {
		return err
	}
	if err := watchlist.Init(meter); err != nil {
		return err
	}
	return nil
}
