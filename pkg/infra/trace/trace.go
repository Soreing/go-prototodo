package trace

import (
	"context"
	"prototodo/pkg/domain/base/logger"
	"prototodo/pkg/infra/tracelib"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ExporterList stores and provides a list of exporters
type ExporterList struct {
	Exporters []sdktrace.SpanExporter
}

// NewTracer constructs a new Tracer
func NewTracer(
	expl *ExporterList,
	opts *Options,
	lgrf logger.IFactory,
) (*tracelib.Tracer, error) {
	lgr := lgrf.Create(context.TODO())

	return tracelib.NewTracer(
		opts.ServiceName,
		expl.Exporters,
		&spanConstructor{},
		&traceExtractor{},
		lgr,
	)
}
