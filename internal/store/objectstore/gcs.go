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
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/ContainerSolutions/factory/internal/clock"
)

// DefaultGCSConfirmEvery is how long a request that proved a GCS bucket exists lets a
// not-found Get or Delete skip its confirming list (GCSBucket).
const DefaultGCSConfirmEvery = time.Minute

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
//     ErrNotExist: gcsBucketMissing's message match is the fast path, and otherwise a
//     one-object list of the prefix confirms the bucket (notFound), at most once per
//     DefaultGCSConfirmEvery: a request that proved the bucket exists within it (a
//     successful Get, Create, List or Delete, a 412 on a create, or a confirmed 404) skips
//     the list. ParseURL lists the prefix once at open, so a bucket that doesn't exist fails
//     the start rather than reading as an empty, current log.
//   - List lists from startOffset, in name order, skipping startAfter itself.
//
// Each request's deadline is its context's: the store gives each one DefaultRequestTimeout.
type GCSBucket struct {
	client *storage.Client
	bucket *storage.BucketHandle
	loc    cloudLocation

	// clk and confirmEvery time the confirming list (gcsOption).
	clk          clock.Clock
	confirmEvery time.Duration
	// confirming holds one token: one confirming list at a time, so callers that find the
	// bucket unproven together send one list between them.
	confirming chan struct{}

	mu sync.Mutex
	// provenAt is when a request last proved the bucket exists (zero: never).
	provenAt time.Time
}

// gcsOption sets a GCSBucket's confirmation timing (tests; ParseURL uses the defaults).
type gcsOption func(*GCSBucket)

// withGCSClock times the confirmation window on clk (default clock.Real).
func withGCSClock(clk clock.Clock) gcsOption { return func(b *GCSBucket) { b.clk = clk } }

// withGCSConfirmEvery sets the confirmation window (default DefaultGCSConfirmEvery).
func withGCSConfirmEvery(d time.Duration) gcsOption {
	return func(b *GCSBucket) { b.confirmEvery = d }
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
func newGCSBucket(ctx context.Context, loc cloudLocation, hc *http.Client, options ...gcsOption) (*GCSBucket, error) {
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
	b := &GCSBucket{
		client: client, bucket: client.Bucket(loc.bucket), loc: loc,
		clk: clock.Real{}, confirmEvery: DefaultGCSConfirmEvery, confirming: make(chan struct{}, 1),
	}
	for _, o := range options {
		o(b)
	}
	return b, nil
}

// proven notes that a request just proved the bucket exists.
func (b *GCSBucket) proven() {
	now := b.clk.Now()
	b.mu.Lock()
	b.provenAt = now
	b.mu.Unlock()
}

// recentlyProven reports a proof within the confirmation window.
func (b *GCSBucket) recentlyProven() bool {
	now := b.clk.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.provenAt.IsZero() && now.Sub(b.provenAt) < b.confirmEvery
}

// probe lists at most one object under the prefix, at open.
func (b *GCSBucket) probe(ctx context.Context) error {
	if err := b.listOne(ctx); err != nil {
		return fmt.Errorf("objectstore: open: %w", err)
	}
	return nil
}

// listOne lists at most one object under the prefix. The SDK maps a list's 404 to
// ErrBucketNotExist whatever GCS's message says, so a missing bucket is an error here
// (gcsBucketMissing), never an empty listing.
func (b *GCSBucket) listOne(ctx context.Context) error {
	it := b.bucket.Objects(ctx, &storage.Query{Prefix: b.loc.prefix, Projection: storage.ProjectionNoACL})
	it.PageInfo().MaxSize = 1
	if _, err := it.Next(); err != nil && !errors.Is(err, iterator.Done) {
		return b.err(ctx, "list", "", err)
	}
	b.proven()
	return nil
}

// notFound maps a Get's or a Delete's error. An object 404 is ErrNotExist only once the
// bucket is known to exist: the SDK gives a missing bucket the same ErrObjectNotExist, and
// gcsBucketMissing's message match is only the fast path, so a bucket that vanishes mid-run
// fails loudly, within the confirmation window, even if GCS rewords its message. When the
// confirming list fails, the outcome is that error, not ErrNotExist.
func (b *GCSBucket) notFound(ctx context.Context, op, name string, err error) error {
	if errors.Is(gcsSentinel(err), ErrNotExist) {
		if lerr := b.confirmBucket(ctx); lerr != nil {
			return fmt.Errorf("gs://%s/%s%s: %s: not found, and the bucket check failed: %w", b.loc.bucket, b.loc.prefix, name, op, lerr)
		}
	}
	return b.err(ctx, op, name, err)
}

// confirmBucket lists one object unless a request proved the bucket exists within the
// confirmation window. One caller lists at a time; the others wait for it (or their ctx)
// and then find the proof, so concurrent not-found requests send one list per window.
//
// "At most once per window" holds while lists succeed. A failed list proves nothing, so
// during a GCS failure every not-found Get or Delete lists again (one at a time) and returns
// the list's error, never ErrNotExist. That is deliberate (Gustavo, 2026-10-28): no cap, so
// a store never reads an unconfirmed 404 as a current log tail.
func (b *GCSBucket) confirmBucket(ctx context.Context) error {
	if b.recentlyProven() {
		return nil
	}
	select {
	case b.confirming <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-b.confirming }()
	if b.recentlyProven() {
		return nil
	}
	return b.listOne(ctx)
}

// Close releases the client's connections.
func (b *GCSBucket) Close() error { return b.client.Close() }

// String is the bucket's URL.
func (b *GCSBucket) String() string { return "gs://" + b.loc.bucket + "/" + b.loc.prefix }

// Get implements Bucket.
func (b *GCSBucket) Get(ctx context.Context, name string) ([]byte, error) {
	r, err := b.bucket.Object(b.loc.prefix + name).NewReader(ctx)
	if err != nil {
		return nil, b.notFound(ctx, "get", name, err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, b.err(ctx, "get", name, err)
	}
	b.proven()
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
		err = b.err(ctx, "create", name, err)
		if errors.Is(err, ErrExists) {
			b.proven() // the object exists, so the bucket does
		}
		return err
	}
	b.proven()
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
			b.proven()
			return nil
		}
		if err != nil {
			return b.err(ctx, "list", prefix, err)
		}
		b.proven()
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
		return b.notFound(ctx, "delete", name, err)
	}
	b.proven()
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
