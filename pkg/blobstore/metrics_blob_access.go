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
	"go.opencensus.io/trace"

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
func NewMetricsBlobAccess(blobAccess BlobAccess, clock clock.Clock, name string) BlobAccess {
	blobAccessOperationsPrometheusMetrics.Do(func() {
		prometheus.MustRegister(blobAccessOperationsBlobSizeBytes)
		prometheus.MustRegister(blobAccessOperationsFindMissingBatchSize)
		prometheus.MustRegister(blobAccessOperationsDurationSeconds)
	})

	return &metricsBlobAccess{
		blobAccess: blobAccess,
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
	ctx, span := trace.StartSpan(ctx, ba.name+".Get")
	span.AddAttributes(
		trace.StringAttribute("digest", digest.String()),
	)

	b := buffer.WithErrorHandler(
		ba.blobAccess.Get(ctx, digest),
		&metricsErrorHandler{
			blobAccess: ba,
			timeStart:  ba.clock.Now(),
			errorCode:  codes.OK,
			span:       span,
		})
	if sizeBytes, err := b.GetSizeBytes(); err == nil {
		ba.getBlobSizeBytes.Observe(float64(sizeBytes))
	}
	return b
}

func (ba *metricsBlobAccess) Put(ctx context.Context, digest digest.Digest, b buffer.Buffer) error {
	ctx, span := trace.StartSpan(ctx, ba.name+".Put")
	span.AddAttributes(
		trace.StringAttribute("digest", digest.String()),
	)
	defer span.End()

	// If the Buffer is in a known error state, return the error
	// here instead of propagating the error to the underlying
	// BlobAccess. Such a Put() call wouldn't have any effect.
	sizeBytes, err := b.GetSizeBytes()
	if err != nil {
		span.SetStatus(errToStatus(err))
		return err
	}
	ba.putBlobSizeBytes.Observe(float64(sizeBytes))

	timeStart := ba.clock.Now()
	err = ba.blobAccess.Put(ctx, digest, b)
	ba.updateDurationSeconds(ba.putDurationSeconds, status.Code(err), timeStart)
	span.SetStatus(errToStatus(err))
	return err
}

func (ba *metricsBlobAccess) FindMissing(ctx context.Context, digests digest.Set) (digest.Set, error) {
	ctx, span := trace.StartSpan(ctx, ba.name+".FindMissing")
	span.AddAttributes(
		trace.Int64Attribute("digests", int64(digests.Length())),
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
	span.SetStatus(errToStatus(err))
	return digests, err
}

type metricsErrorHandler struct {
	blobAccess *metricsBlobAccess
	timeStart  time.Time
	errorCode  codes.Code
	span       *trace.Span
}

func (eh *metricsErrorHandler) OnError(err error) (buffer.Buffer, error) {
	eh.errorCode = status.Code(err)
	eh.span.SetStatus(errToStatus(err))
	return nil, err
}

func (eh *metricsErrorHandler) Done() {
	eh.blobAccess.updateDurationSeconds(eh.blobAccess.getDurationSeconds, eh.errorCode, eh.timeStart)
	eh.span.End()
}

func errToStatus(err error) trace.Status {
	if err == nil {
		return trace.Status{}
	}
	return trace.Status{Code: int32(status.Code(err)), Message: err.Error()}
}
