package batching

import (
	"time"
	"fmt"
	"context"

	prom "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/metric"
	api "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/attribute"
)

const (
	subsystem    = "tattler"
	successLabel = "success"
	sourceTypeLabel = "source_type"
)

var (
	// Metric Registry = NewRegistry()
)

func metricName(name string) string {
	return fmt.Sprintf("%s_%s", subsystem, name)
}

// NewRegistry creates a new Registry with initialized prometheus counter definitions
func NewRegistry(meter api.Meter) (Registry, error) {
	batchesEmittedCount, err := meter.Float64Counter(metricName("batches_emitted_total"), api.WithDescription("total number of batches emitted by tattler"))
	if err != nil {
		return Registry{}, err
	}
	currentBatchSize, err := meter.Float64ObservableGauge(metricName("current_batch_size"), api.WithDescription("current size of batch emitted by tattler"))
	if err != nil {
		return Registry{}, err
	}
	return Registry{
		// batchesEmittedCount: prom.NewCounterVec(prom.CounterOpts{
		// 	Name:      "batches_emitted_total",
		// 	Help:      "total number of events sent by the ARN client",
		// 	Subsystem: subsystem,
		// }, []string{successLabel, "source_type"}),
		batchesEmittedCount: batchesEmittedCount,
		batchItemsEmittedCount: prom.NewCounterVec(prom.CounterOpts{
			Name:      "batch_items_emitted_total",
			Help:      "total number of events sent by the ARN client",
			Subsystem: subsystem,
		}, []string{successLabel, "source_type"}),
		eventSentLatency: prom.NewHistogramVec(prom.HistogramOpts{
			Name:      "event_sent_seconds",
			Help:      "latency distributions of events sent by the ARN client",
			Subsystem: subsystem,
			Buckets: []float64{0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 1.25, 1.5, 2, 3,
				4, 5, 6, 8, 10, 15, 20, 30, 45, 60},
		}, []string{}),
		// currentBatchSize: prom.NewGaugeVec(prom.GaugeOpts{
		// 	Name:      "current_batch_size",
		// 	Help:      "total number of events sent by the ARN client",
		// 	Subsystem: subsystem,
		// }, []string{"source_type"}),
		currentBatchSize: currentBatchSize,
	}, nil
}

func (m *Registry) Init(reg prom.Registerer) {
	reg.MustRegister(
		// m.batchesEmittedCount,
		m.eventSentLatency,
	)
}

// Registry provides the prometheus metrics for the message processor
type Registry struct {
	// batchesEmittedCount           *prom.CounterVec
	batchesEmittedCount metric.Float64Counter
	batchItemsEmittedCount           *prom.CounterVec
	eventSentLatency            *prom.HistogramVec
	// currentBatchSize         *prom.GaugeVec
	currentBatchSize         metric.Float64ObservableGauge
}

// RecordSendEventSuccess increases the eventSentCount metric with success == true
// and records the latency
func (m *Registry)  RecordSendEventSuccess(ctx context.Context, elapsed time.Duration) {
	opt := api.WithAttributes(
		attribute.Key(sourceTypeLabel).Bool(true),
	)
	m.batchesEmittedCount.Add(ctx, 1, opt)
	// eventSentCount.With(
	// 	prom.Labels{
	// 		successLabel: "true",
	// 	}).Inc()
	// eventSentLatency.WithLabelValues().Observe(elapsed.Seconds())
}

// RecordSendEventFailure increases the eventSentCount metric with success == false
// and records the latency
func RecordSendEventFailure(elapsed time.Duration) {
	// eventSentCount.With(
	// 	prom.Labels{
	// 		successLabel: "false",
	// 	}).Inc()
	// eventSentLatency.WithLabelValues().Observe(elapsed.Seconds())
}

// Reset resets the metrics
func Reset() {
	// eventSentCount.Reset()
	// eventSentLatency.Reset()
}
