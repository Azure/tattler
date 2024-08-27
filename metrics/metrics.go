package metrics

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"

	"go.opentelemetry.io/otel"
	"github.com/prometheus/client_golang/prometheus"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	api "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Azure/tattler/metrics/batching"
	"github.com/Azure/tattler/metrics/watchlist"
)

var serviceName attribute.KeyValue

// InitTelemetry initializes the opentelemetry resources with the given service name and Prometheus registry.
// This only sets up metrics currently. Tracing can be added later.
func InitTelemetry(parentCtx context.Context, logger *slog.Logger, service string, reg prometheus.Registerer) error {
	logger.Info("Waiting for connection...")

	ctx, cancel := signal.NotifyContext(parentCtx, os.Interrupt)
	defer cancel()

	serviceName = semconv.ServiceNameKey.String(service)
	res, err := resource.New(ctx,
		resource.WithAttributes(
			// The service name used to display traces in backends
			serviceName,
		),
	)
	if err != nil {
		return fmt.Errorf("failed to create resource: %w", err)
	}

	shutdownMeterProvider, err := InitMeterProvider(res, reg)
	if err != nil {
		return fmt.Errorf("failed to initialize meter provider: %w", err)
	}

	go func() {
		<-parentCtx.Done()
		log.Printf("Shutting down MeterProvider")
		if err := shutdownMeterProvider(ctx); err != nil {
			logger.Error("failed to shutdown MeterProvider: %s", err)
			return
		}
	}()

	return nil
}

// InitMeterProvider initializes a Prometheus exporter, and configures the corresponding meter provider.
func InitMeterProvider(res *resource.Resource, reg prometheus.Registerer) (func(context.Context) error, error) {
	metricExporter, err := otelprometheus.New(otelprometheus.WithRegisterer(reg))
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics exporter: %w", err)
	}

	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(metricExporter),
		sdkmetric.WithResource(res),
	)
	// use global meter provider
	otel.SetMeterProvider(meterProvider)

	meter := meterProvider.Meter("tattler")
	if err := batching.Init(meter); err != nil {
		return nil, err
	}
	if err := watchlist.Init(meter); err != nil {
		return nil, err
	}

	return meterProvider.Shutdown, nil
}

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
