package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// GCSBucket is a Bucket on Google Cloud Storage (gs://bucket/prefix). Credentials are
// Application Default Credentials (Cloud Run's service account); none come from a flag or the
// config. STORAGE_EMULATOR_HOST (the SDK's own variable) points it at an emulator such as
// fake-gcs-server instead, without credentials.
//
// Every call goes through the JSON API, reads included, so one set of errors applies:
//   - Create uploads with ifGenerationMatch=0 in one request; a 412 is ErrExists. The SDK
//     retries it on a 408, 429, 5xx or a broken connection (a conditional upload is
//     idempotent), so a retry whose first attempt landed answers ErrExists, which the
//     store's nonce check resolves.
//   - Put uploads unconditionally (the SDK doesn't retry it).
//   - Get and Delete answer ErrNotExist on a 404.
//   - List lists from startOffset, in name order, skipping startAfter itself.
//
// Each request's deadline is its context's: the store gives each one DefaultRequestTimeout.
type GCSBucket struct {
	client *storage.Client
	bucket *storage.BucketHandle
	loc    cloudLocation
}

var _ Bucket = (*GCSBucket)(nil)

func openGCSURL(ctx context.Context, u *url.URL) (Bucket, error) {
	loc, err := parseCloudURL(u)
	if err != nil {
		return nil, err
	}
	return newGCSBucket(ctx, loc, nil)
}

// newGCSBucket opens loc; hc, when set, is the client's HTTP client (tests wrap the transport
// to fail a request after it landed).
func newGCSBucket(ctx context.Context, loc cloudLocation, hc *http.Client) (*GCSBucket, error) {
	opts := []option.ClientOption{storage.WithJSONReads()}
	if hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("objectstore: gs://%s: %w", loc.bucket, err)
	}
	return &GCSBucket{client: client, bucket: client.Bucket(loc.bucket), loc: loc}, nil
}

// Close releases the client's connections.
func (b *GCSBucket) Close() error { return b.client.Close() }

// String is the bucket's URL.
func (b *GCSBucket) String() string { return "gs://" + b.loc.bucket + "/" + b.loc.prefix }

// Get implements Bucket.
func (b *GCSBucket) Get(ctx context.Context, name string) ([]byte, error) {
	r, err := b.bucket.Object(b.loc.prefix + name).NewReader(ctx)
	if err != nil {
		return nil, b.err(ctx, "get", name, err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, b.err(ctx, "get", name, err)
	}
	return data, nil
}

// Create implements Bucket: an upload with ifGenerationMatch=0.
func (b *GCSBucket) Create(ctx context.Context, name string, data []byte) error {
	return b.write(ctx, "create", name, data, true)
}

// Put implements Bucket: an unconditional upload.
func (b *GCSBucket) Put(ctx context.Context, name string, data []byte) error {
	return b.write(ctx, "put", name, data, false)
}

func (b *GCSBucket) write(ctx context.Context, op, name string, data []byte, ifAbsent bool) error {
	if name == "" {
		return fmt.Errorf("gs: %s: empty object name", op)
	}
	obj := b.bucket.Object(b.loc.prefix + name)
	if ifAbsent {
		obj = obj.If(storage.Conditions{DoesNotExist: true})
	}
	// The SDK's writer runs the upload on a goroutine of its own: cancel it if Write fails, so
	// it doesn't outlive this call.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := obj.NewWriter(ctx)
	// A chunk that holds the whole object: the SDK buffers it and sends one multipart request,
	// which it can send again (ChunkSize 0 streams the body, and the SDK never retries that).
	// No resumable session is opened, so none is left behind.
	w.ChunkSize = len(data) + 1
	w.ContentType = "application/octet-stream"
	if _, err := w.Write(data); err != nil {
		cancel()
		_ = w.Close()
		return b.err(ctx, op, name, err)
	}
	if err := w.Close(); err != nil {
		return b.err(ctx, op, name, err)
	}
	return nil
}

// List implements Bucket. GCS's startOffset is inclusive, so startAfter itself is skipped.
func (b *GCSBucket) List(ctx context.Context, prefix, startAfter string, fn func(ObjectInfo) error) error {
	q := &storage.Query{Prefix: b.loc.prefix + prefix, Projection: storage.ProjectionNoACL}
	if startAfter != "" {
		q.StartOffset = b.loc.prefix + startAfter
	}
	if err := q.SetAttrSelection([]string{"Name", "Size", "Updated"}); err != nil {
		return fmt.Errorf("gs: list %s: %w", prefix, err)
	}
	it := b.bucket.Objects(ctx, q)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return b.err(ctx, "list", prefix, err)
		}
		name, ok := strings.CutPrefix(attrs.Name, b.loc.prefix)
		if !ok {
			return fmt.Errorf("gs://%s: list %s: object %q is outside the prefix", b.loc.bucket, prefix, attrs.Name)
		}
		if name == startAfter {
			continue
		}
		if err := fn(ObjectInfo{Name: name, Size: attrs.Size, Updated: attrs.Updated}); err != nil {
			return err
		}
	}
}

// Delete implements Bucket.
func (b *GCSBucket) Delete(ctx context.Context, name string) error {
	if err := b.bucket.Object(b.loc.prefix + name).Delete(ctx); err != nil {
		return b.err(ctx, "delete", name, err)
	}
	return nil
}

// err maps a GCS error to the Bucket's: a missing object (404) is ErrNotExist, a failed
// ifGenerationMatch=0 (412) is ErrExists. Anything else is returned as it is, matching ctx's
// error when the context ended.
func (b *GCSBucket) err(ctx context.Context, op, name string, err error) error {
	if mapped := gcsSentinel(err); mapped != nil {
		err = mapped
	} else {
		err = withCtxErr(ctx, err)
	}
	return fmt.Errorf("gs://%s/%s%s: %s: %w", b.loc.bucket, b.loc.prefix, name, op, err)
}

// gcsSentinel is ErrNotExist or ErrExists for the errors that mean them, nil otherwise. A
// missing bucket is not ErrNotExist: the SDK's ErrBucketNotExist, or a 404 it left as it is,
// is a configuration error, not a missing object.
func gcsSentinel(err error) error {
	if errors.Is(err, storage.ErrObjectNotExist) {
		return ErrNotExist
	}
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok && gerr.Code == http.StatusPreconditionFailed {
		return ErrExists
	}
	return nil
}
