package builder

import (
	"context"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"go.opentelemetry.io/otel/api/trace"
	"go.opentelemetry.io/otel/label"
)

type tracingBuildQueue struct {
	base BuildQueue
}

// NewTracingBuildQueue injects BuildQueue annotations into trace spans
func NewTracingBuildQueue(base BuildQueue) BuildQueue {
	return &tracingBuildQueue{
		base: base,
	}
}

func (bq *tracingBuildQueue) GetCapabilities(ctx context.Context, in *remoteexecution.GetCapabilitiesRequest) (*remoteexecution.ServerCapabilities, error) {
	trace.SpanFromContext(ctx).AddEvent(ctx, "BuildQueue.GetCapabilities",
		label.String("instance", in.InstanceName),
	)
	return bq.base.GetCapabilities(ctx, in)
}

func (bq *tracingBuildQueue) Execute(in *remoteexecution.ExecuteRequest, out remoteexecution.Execution_ExecuteServer) error {
	trace.SpanFromContext(out.Context()).AddEvent(out.Context(), "BuildQueue.Execute",
		label.String("instance", in.InstanceName),
		label.String("digest", in.ActionDigest.Hash),
	)
	return bq.base.Execute(in, out)
}

func (bq *tracingBuildQueue) WaitExecution(in *remoteexecution.WaitExecutionRequest, out remoteexecution.Execution_WaitExecutionServer) error {
	trace.SpanFromContext(out.Context()).AddEvent(out.Context(), "BuildQueue.WaitExecution",
		label.String("name", in.Name),
	)
	return bq.base.WaitExecution(in, out)
}
