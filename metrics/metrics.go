package metrics

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"github.com/prometheus/client_golang/prometheus"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Azure/tattler/metrics/batching"
	"github.com/Azure/tattler/metrics/watchlist"
)

var serviceName attribute.KeyValue

// use this as an example instead, services should decide what they want to initialize
func InitTelemetry(ctx context.Context, logger *slog.Logger, service string, reg prometheus.Registerer) error {
	logger.Info("Waiting for connection...")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
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
		<-ctx.Done()
		log.Printf("Shutting down MeterProvider")
		if err := shutdownMeterProvider(ctx); err != nil {
			logger.Error("failed to shutdown MeterProvider: %s", err)
			return
		}
	}()

	return nil
}

// Initialize a gRPC connection to be used by both the tracer and meter
// providers.
func InitConn() (*grpc.ClientConn, error) {
	// It connects the OpenTelemetry Collector through local gRPC connection.
	// You may replace `localhost:4317` with your endpoint.
	conn, err := grpc.NewClient("localhost:4317",
		// Note the use of insecure transport here. TLS is recommended in production.
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection to collector: %w", err)
	}

	return conn, err
}

// Initializes an OTLP exporter, and configures the corresponding trace provider.
func InitTracerProvider(ctx context.Context, res *resource.Resource, conn *grpc.ClientConn) (func(context.Context) error, error) {
	// Set up a trace exporter
	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	// Register the trace exporter with a TracerProvider, using a batch
	// span processor to aggregate spans before export.
	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)
	otel.SetTracerProvider(tracerProvider)

	// Set global propagator to tracecontext (the default is no-op).
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Shutdown will flush any remaining spans and shut down the exporter.
	return tracerProvider.Shutdown, nil
}

// Initializes a Prometheus exporter, and configures the corresponding meter provider.
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
