package objectstore

import (
	"context"
	"encoding/json"
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
//   - Get and Delete answer ErrNotExist on a missing object's 404. A missing bucket is not
//     ErrNotExist (gcsBucketMissing), and ParseURL lists the prefix once at open, so a bucket
//     that doesn't exist fails the start rather than reading as an empty, current log.
//   - List lists from startOffset, in name order, skipping startAfter itself.
//
// Each request's deadline is its context's: the store gives each one DefaultRequestTimeout.
type GCSBucket struct {
	client *storage.Client
	bucket *storage.BucketHandle
	loc    cloudLocation
}

var _ Bucket = (*GCSBucket)(nil)

// openGCSURL opens the bucket and lists one object under its prefix with ctx, so a missing
// bucket, or one this identity can't read, fails here.
func openGCSURL(ctx context.Context, u *url.URL) (Bucket, error) {
	loc, err := parseCloudURL(u)
	if err != nil {
		return nil, err
	}
	b, err := newGCSBucket(ctx, loc, nil)
	if err != nil {
		return nil, err
	}
	if err := b.probe(ctx); err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

// newGCSBucket opens loc; hc, when set, is the client's HTTP client (tests wrap the transport
// to fail a request after it landed).
func newGCSBucket(ctx context.Context, loc cloudLocation, hc *http.Client) (*GCSBucket, error) {
	opts := []option.ClientOption{storage.WithJSONReads()}
	if hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	// Some credentials keep NewClient's context for every later token refresh (x/oauth2's
	// sources for authorized_user and external_account ADC). ctx only bounds the opening, so
	// the client gets one that is never cancelled; each request has its own context.
	client, err := storage.NewClient(context.WithoutCancel(ctx), opts...)
	if err != nil {
		return nil, fmt.Errorf("objectstore: gs://%s: %w", loc.bucket, err)
	}
	return &GCSBucket{client: client, bucket: client.Bucket(loc.bucket), loc: loc}, nil
}

// probe lists at most one object under the prefix.
func (b *GCSBucket) probe(ctx context.Context) error {
	it := b.bucket.Objects(ctx, &storage.Query{Prefix: b.loc.prefix, Projection: storage.ProjectionNoACL})
	it.PageInfo().MaxSize = 1
	if _, err := it.Next(); err != nil && !errors.Is(err, iterator.Done) {
		return fmt.Errorf("objectstore: open: %w", b.err(ctx, "list", "", err))
	}
	return nil
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
	if name == "" {
		return errors.New("gs: create: empty object name")
	}
	obj := b.bucket.Object(b.loc.prefix + name).If(storage.Conditions{DoesNotExist: true})
	// The SDK's writer runs the upload on a goroutine of its own: cancel it if Write fails, so
	// it doesn't outlive this call.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := obj.NewWriter(wctx)
	// A chunk that holds the whole object: the SDK buffers it and sends one multipart request,
	// which it can send again (ChunkSize 0 streams the body, and the SDK never retries that).
	// No resumable session is opened, so none is left behind.
	w.ChunkSize = len(data) + 1
	w.ContentType = "application/octet-stream"
	if _, err := w.Write(data); err != nil {
		cancel()
		_ = w.Close()
		return b.err(ctx, "create", name, err)
	}
	if err := w.Close(); err != nil {
		return b.err(ctx, "create", name, err)
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

// err maps a GCS error to the Bucket's (gcsSentinel), keeping the SDK's error in the chain.
// Anything else is returned as it is, matching ctx's error when the context ended.
func (b *GCSBucket) err(ctx context.Context, op, name string, err error) error {
	switch mapped := gcsSentinel(err); {
	case mapped != nil:
		err = fmt.Errorf("%w: %w", mapped, err)
	case gcsBucketMissing(err):
		err = fmt.Errorf("the bucket does not exist: %w", err)
	default:
		err = withCtxErr(ctx, err)
	}
	return fmt.Errorf("gs://%s/%s%s: %s: %w", b.loc.bucket, b.loc.prefix, name, op, err)
}

// gcsSentinel is ErrNotExist or ErrExists for the errors that mean them, nil otherwise:
//   - a missing object (the SDK's ErrObjectNotExist) is ErrNotExist, unless the bucket is
//     what is missing (gcsBucketMissing): that is a configuration error, and as ErrNotExist
//     a vanished bucket would read as a current, empty log tail;
//   - a failed ifGenerationMatch=0 (412) is ErrExists.
func gcsSentinel(err error) error {
	if gcsBucketMissing(err) {
		return nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return ErrNotExist
	}
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok && gerr.Code == http.StatusPreconditionFailed {
		return ErrExists
	}
	return nil
}

// gcsBucketMissing reports a 404 about the bucket rather than the object. storage v1.70.0
// turns every 404 from objects.get and objects.delete into ErrObjectNotExist, and GCS gives
// both the same reason ("notFound") and domain ("global"), so only the JSON error's message
// tells them apart: "The specified bucket does not exist." (an object's is "No such object:
// …"). A media download (Get) leaves the JSON unparsed in the error's Body, so that is read
// too. The SDK's own ErrBucketNotExist (bucket calls) counts as well.
func gcsBucketMissing(err error) bool {
	if errors.Is(err, storage.ErrBucketNotExist) {
		return true
	}
	gerr, ok := errors.AsType[*googleapi.Error](err)
	if !ok || gerr.Code != http.StatusNotFound {
		return false
	}
	messages := []string{gerr.Message}
	for _, item := range gerr.Errors {
		messages = append(messages, item.Message)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(gerr.Body), &body) == nil {
		messages = append(messages, body.Error.Message)
		for _, item := range body.Error.Errors {
			messages = append(messages, item.Message)
		}
	}
	for _, m := range messages {
		if strings.Contains(strings.ToLower(m), "bucket does not exist") {
			return true
		}
	}
	return false
}
