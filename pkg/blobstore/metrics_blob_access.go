package blobstore

import (
	"context"
	"sync"
	"time"

	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/clock"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/api/trace"
	"go.opentelemetry.io/otel/label"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	blobAccessOperationsPrometheusMetrics sync.Once

	blobAccessOperationsBlobSizeBytes = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "buildbarn",
			Subsystem: "blobstore",
			Name:      "blob_access_operations_blob_size_bytes",
			Help:      "Size of blobs being inserted/retrieved, in bytes.",
			Buckets:   prometheus.ExponentialBuckets(1.0, 2.0, 33),
		},
		[]string{"name", "operation"})
	blobAccessOperationsFindMissingBatchSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "buildbarn",
			Subsystem: "blobstore",
			Name:      "blob_access_operations_find_missing_batch_size",
			Help:      "Number of digests provided to FindMissing().",
			Buckets:   prometheus.ExponentialBuckets(1.0, 2.0, 17),
		},
		[]string{"name"})
	blobAccessOperationsDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "buildbarn",
			Subsystem: "blobstore",
			Name:      "blob_access_operations_duration_seconds",
			Help:      "Amount of time spent per operation on blob access objects, in seconds.",
			Buckets:   util.DecimalExponentialBuckets(-3, 6, 2),
		},
		[]string{"name", "operation", "grpc_code"})
)

type metricsBlobAccess struct {
	blobAccess BlobAccess
	tracer     trace.Tracer
	clock      clock.Clock
	name       string

	getBlobSizeBytes           prometheus.Observer
	getDurationSeconds         prometheus.ObserverVec
	putBlobSizeBytes           prometheus.Observer
	putDurationSeconds         prometheus.ObserverVec
	findMissingBatchSize       prometheus.Observer
	findMissingDurationSeconds prometheus.ObserverVec
}

// NewMetricsBlobAccess creates an adapter for BlobAccess that adds
// basic instrumentation in the form of Prometheus metrics.
func NewMetricsBlobAccess(blobAccess BlobAccess, tracer trace.Tracer, clock clock.Clock, name string) BlobAccess {
	blobAccessOperationsPrometheusMetrics.Do(func() {
		prometheus.MustRegister(blobAccessOperationsBlobSizeBytes)
		prometheus.MustRegister(blobAccessOperationsFindMissingBatchSize)
		prometheus.MustRegister(blobAccessOperationsDurationSeconds)
	})

	return &metricsBlobAccess{
		blobAccess: blobAccess,
		tracer:     tracer,
		clock:      clock,
		name:       name,

		getBlobSizeBytes:           blobAccessOperationsBlobSizeBytes.WithLabelValues(name, "Get"),
		getDurationSeconds:         blobAccessOperationsDurationSeconds.MustCurryWith(map[string]string{"name": name, "operation": "Get"}),
		putBlobSizeBytes:           blobAccessOperationsBlobSizeBytes.WithLabelValues(name, "Put"),
		putDurationSeconds:         blobAccessOperationsDurationSeconds.MustCurryWith(map[string]string{"name": name, "operation": "Put"}),
		findMissingBatchSize:       blobAccessOperationsFindMissingBatchSize.WithLabelValues(name),
		findMissingDurationSeconds: blobAccessOperationsDurationSeconds.MustCurryWith(map[string]string{"name": name, "operation": "FindMissing"}),
	}
}

func (ba *metricsBlobAccess) updateDurationSeconds(vec prometheus.ObserverVec, code codes.Code, timeStart time.Time) {
	vec.WithLabelValues(code.String()).Observe(ba.clock.Now().Sub(timeStart).Seconds())
}

func (ba *metricsBlobAccess) Get(ctx context.Context, digest digest.Digest) buffer.Buffer {
	// This span covers the entire process of reading the
	// buffer. span.End() will be called in metricsErrorHandler.Done.
	ctx, span := ba.tracer.Start(ctx, ba.name+".Get",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			label.String("blobaccess.digest", digest.String()),
		),
	)

	b := ba.blobAccess.Get(ctx, digest)
	span.AddEvent(ctx, ".Get call complete")

	b = buffer.WithErrorHandler(b, &metricsErrorHandler{
		blobAccess: ba,
		timeStart:  ba.clock.Now(),
		errorCode:  codes.OK,
		ctx:        ctx,
	})
	if sizeBytes, err := b.GetSizeBytes(); err == nil {
		ba.getBlobSizeBytes.Observe(float64(sizeBytes))
	}
	return b
}

func (ba *metricsBlobAccess) Put(ctx context.Context, digest digest.Digest, b buffer.Buffer) error {
	ctx, span := ba.tracer.Start(ctx, ba.name+".Put",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			label.String("blobaccess.digest", digest.String()),
		),
	)
	defer span.End()

	// If the Buffer is in a known error state, return the error
	// here instead of propagating the error to the underlying
	// BlobAccess. Such a Put() call wouldn't have any effect.
	sizeBytes, err := b.GetSizeBytes()
	if err != nil {
		util.RecordError(ctx, err)
		return err
	}
	ba.putBlobSizeBytes.Observe(float64(sizeBytes))

	timeStart := ba.clock.Now()
	err = ba.blobAccess.Put(ctx, digest, b)
	ba.updateDurationSeconds(ba.putDurationSeconds, status.Code(err), timeStart)
	util.RecordError(ctx, err)
	return err
}

func (ba *metricsBlobAccess) FindMissing(ctx context.Context, digests digest.Set) (digest.Set, error) {
	ctx, span := ba.tracer.Start(ctx, ba.name+".FindMissing",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			label.Int("blobaccess.digest_count", digests.Length()),
		),
	)
	defer span.End()

	// Discard zero-sized FindMissing() requests. These may, for
	// example, be generated by SizeDistinguishingBlobAccess. Such
	// calls would skew the batch size and duration metrics.
	if digests.Empty() {
		return digest.EmptySet, nil
	}

	ba.findMissingBatchSize.Observe(float64(digests.Length()))
	timeStart := ba.clock.Now()
	digests, err := ba.blobAccess.FindMissing(ctx, digests)
	ba.updateDurationSeconds(ba.findMissingDurationSeconds, status.Code(err), timeStart)
	util.RecordError(ctx, err)
	return digests, err
}

type metricsErrorHandler struct {
	blobAccess *metricsBlobAccess
	timeStart  time.Time
	errorCode  codes.Code
	ctx        context.Context
}

func (eh *metricsErrorHandler) OnError(err error) (buffer.Buffer, error) {
	eh.errorCode = status.Code(err)
	util.RecordError(eh.ctx, err)
	return nil, err
}

func (eh *metricsErrorHandler) Done() {
	eh.blobAccess.updateDurationSeconds(eh.blobAccess.getDurationSeconds, eh.errorCode, eh.timeStart)
	trace.SpanFromContext(eh.ctx).End()
}
