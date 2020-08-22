package circular

import (
	"context"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"sync"
	"time"

	"github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.opentelemetry.io/otel/api/trace"
	"go.opentelemetry.io/otel/label"
)

// OffsetStore maps a digest to an offset within the data file. This is
// where the blob's contents may be found.
type OffsetStore interface {
	Get(digest digest.Digest, cursors Cursors) (uint64, int64, bool, error)
	Put(digest digest.Digest, offset uint64, length int64, cursors Cursors) error
}

// DataStore is where the data corresponding with a blob is stored. Data
// can be accessed by providing an offset within the data store and its
// length.
type DataStore interface {
	Put(r io.Reader, offset uint64) error
	Get(offset uint64, size int64) io.Reader
}

// StateStore is where global metadata of the circular storage backend
// is stored, namely the read/write cursors where data is currently
// being stored in the data file.
type StateStore interface {
	GetCursors() Cursors
	Allocate(sizeBytes int64) (uint64, error)
	Invalidate(offset uint64, sizeBytes int64) error
}

type circularBlobAccess struct {
	// Fields that are constant or lockless.
	dataStore         DataStore
	readBufferFactory blobstore.ReadBufferFactory
	tracer            trace.Tracer

	// Fields protected by the lock.
	lock        sync.Mutex
	offsetStore OffsetStore
	stateStore  StateStore
}

// NewCircularBlobAccess creates a new circular storage backend. Instead
// of writing data to storage directly, all three storage files are
// injected through separate interfaces.
func NewCircularBlobAccess(offsetStore OffsetStore, dataStore DataStore, stateStore StateStore, readBufferFactory blobstore.ReadBufferFactory, tracer trace.Tracer) blobstore.BlobAccess {
	return &circularBlobAccess{
		offsetStore:       offsetStore,
		dataStore:         dataStore,
		stateStore:        stateStore,
		readBufferFactory: readBufferFactory,
		tracer:            tracer,
	}
}

func (ba *circularBlobAccess) Get(ctx context.Context, digest digest.Digest) buffer.Buffer {
	ctx, span := ba.tracer.Start(ctx, "circularBlobAccess.Get",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			label.String("blobaccess.digest", digest.String()),
		),
	)
	defer span.End()

	ba.lock.Lock()
	startTime := time.Now()
	cursors := ba.stateStore.GetCursors()
	offset, length, ok, err := ba.offsetStore.Get(digest, cursors)
	endTime := time.Now()
	ba.lock.Unlock()

	// Add span events without the lock held.
	span.AddEventWithTimestamp(ctx, startTime, "Lock obtained, calling GetCursors")
	span.AddEventWithTimestamp(ctx, endTime, "offsetStore.Get completed",
		label.Uint64("offset", offset),
		label.Int64("length", length),
		label.Bool("object_found", ok),
	)

	if err == nil && !ok {
		err = status.Errorf(codes.NotFound, "Blob not found")
	}
	util.RecordError(ctx, err)
	if err != nil {
		return buffer.NewBufferFromError(err)
	}

	return ba.readBufferFactory.NewBufferFromReader(
		digest,
		ioutil.NopCloser(ba.dataStore.Get(offset, length)),
		func(dataIsValid bool) {
			if !dataIsValid {
				ba.lock.Lock()
				err := ba.stateStore.Invalidate(offset, length)
				defer ba.lock.Unlock()
				if err == nil {
					log.Printf("Blob %#v was malformed and has been deleted successfully", digest.String())
				} else {
					log.Printf("Blob %#v was malformed and could not be deleted: %s", digest.String(), err)
				}
			}
		})
}

func (ba *circularBlobAccess) Put(ctx context.Context, digest digest.Digest, b buffer.Buffer) error {
	sizeBytes, err := b.GetSizeBytes()
	if err != nil {
		b.Discard()
		return err
	}

	// TODO: This would be more efficient if it passed the buffer
	// down, so IntoWriter() could be used.
	r := b.ToReader()
	defer r.Close()

	ctx, span := ba.tracer.Start(ctx, "circularBlobAccess.Put",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			label.String("blobaccess.digest", digest.String()),
			label.Int64("size_bytes", sizeBytes),
		),
	)
	defer span.End()

	// Allocate space in the data store.
	ba.lock.Lock()
	startTime := time.Now()
	offset, err := ba.stateStore.Allocate(sizeBytes)
	endTime := time.Now()
	ba.lock.Unlock()

	span.AddEventWithTimestamp(ctx, startTime, "Lock obtained")
	span.AddEventWithTimestamp(ctx, endTime, "stateStore.Allocate completed",
		label.Uint64("offset", offset),
	)
	util.RecordError(ctx, err)

	if err != nil {
		return err
	}

	// Write the data to storage.
	if err := ba.dataStore.Put(r, offset); err != nil {
		util.RecordError(ctx, err)
		return err
	}

	var putTime time.Time

	span.AddEvent(ctx, "Obtaining lock")
	ba.lock.Lock()
	startTime = time.Now()
	cursors := ba.stateStore.GetCursors()
	if cursors.Contains(offset, sizeBytes) {
		putTime = time.Now()
		err = ba.offsetStore.Put(digest, offset, sizeBytes, cursors)
	} else {
		err = errors.New("Data became stale before write completed")
	}
	ba.lock.Unlock()

	span.AddEventWithTimestamp(ctx, startTime, "Lock obtained, calling GetCursors")
	if !putTime.IsZero() {
		span.AddEventWithTimestamp(ctx, putTime, "Updating offsetStore")
	}
	util.RecordError(ctx, err)

	return err
}

func (ba *circularBlobAccess) FindMissing(ctx context.Context, digests digest.Set) (digest.Set, error) {
	ba.lock.Lock()
	defer ba.lock.Unlock()

	cursors := ba.stateStore.GetCursors()
	missingDigests := digest.NewSetBuilder()
	for _, blobDigest := range digests.Items() {
		if _, _, ok, err := ba.offsetStore.Get(blobDigest, cursors); err != nil {
			return digest.EmptySet, err
		} else if !ok {
			missingDigests.Add(blobDigest)
		}
	}
	return missingDigests.Build(), nil
}
