package metrics

import (
	api "go.opentelemetry.io/otel/metric"

	"github.com/Azure/tattler/metrics/batching"
	"github.com/Azure/tattler/metrics/readers"
)

// Init initializes all tattler metrics.
func Init(meter api.Meter) error {
	if err := batching.Init(meter); err != nil {
		return err
	}
	if err := readers.Init(meter); err != nil {
		return err
	}
	return nil
}
