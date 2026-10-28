package objectstore

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"google.golang.org/api/googleapi"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/storetest"
)

// The emulator tests run against fake-gcs-server and MinIO in Docker: `make e2e-objectstore`
// starts both, sets these variables and runs TestEmulators. They are opt-in: without
// FACTORY_E2E_OBJECTSTORE=1 they skip; with it, a missing endpoint fails them.
//
//   - FACTORY_E2E_OBJECTSTORE=1
//   - STORAGE_EMULATOR_HOST=host:port (fake-gcs-server; the GCS SDK's own variable)
//   - FACTORY_S3_ENDPOINT=http://host:port (MinIO), with AWS_ACCESS_KEY_ID,
//     AWS_SECRET_ACCESS_KEY and AWS_REGION set to MinIO's test user
//
// The Makefile's E2E_OBJECTSTORE_* variables and envObjectStore are changed together.
const (
	envObjectStore = "FACTORY_E2E_OBJECTSTORE"
	// emulatorBucket is the bucket each emulator test creates (if it is missing) and uses,
	// each test under a prefix of its own.
	emulatorBucket = "factory-e2e"
)

// emulator is one backend under test.
type emulator struct {
	scheme string
	// open opens the emulator's bucket under prefix, through hc when it is set.
	open func(t *testing.T, prefix string, hc *http.Client) Bucket
	// isCreate tells a conditional create's request.
	isCreate func(*http.Request) bool
}

func emulators(t *testing.T) []emulator {
	t.Helper()
	if os.Getenv(envObjectStore) != "1" {
		t.Skipf("set %s=1, STORAGE_EMULATOR_HOST and %s (make e2e-objectstore)", envObjectStore, S3EndpointEnv)
	}
	for _, v := range []string{"STORAGE_EMULATOR_HOST", S3EndpointEnv, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_REGION"} {
		if os.Getenv(v) == "" {
			t.Fatalf("%s=1 but %s is not set", envObjectStore, v)
		}
	}
	ensureGCSBucket(t)
	ensureS3Bucket(t)
	return []emulator{
		{
			scheme: "gs",
			open: func(t *testing.T, prefix string, hc *http.Client) Bucket {
				t.Helper()
				b, err := newGCSBucket(t.Context(), cloudLocation{bucket: emulatorBucket, prefix: prefix}, hc)
				must(t, err)
				t.Cleanup(func() { _ = b.Close() })
				return b
			},
			// One multipart upload: a resumable session (a POST, then a PUT) would count the
			// same and leave sessions behind.
			isCreate: func(r *http.Request) bool {
				q := r.URL.Query()
				return r.Method == http.MethodPost && q.Get("ifGenerationMatch") == "0" && q.Get("uploadType") == "multipart"
			},
		},
		{
			scheme: "s3",
			open: func(t *testing.T, prefix string, hc *http.Client) Bucket {
				t.Helper()
				b, err := newS3Bucket(t.Context(), cloudLocation{bucket: emulatorBucket, prefix: prefix}, hc)
				must(t, err)
				return b
			},
			isCreate: func(r *http.Request) bool {
				return r.Method == http.MethodPut && r.Header.Get("If-None-Match") == "*"
			},
		},
	}
}

func ensureGCSBucket(t *testing.T) {
	t.Helper()
	c, err := storage.NewClient(t.Context())
	must(t, err)
	defer func() { _ = c.Close() }()
	err = c.Bucket(emulatorBucket).Create(t.Context(), "factory-e2e", nil)
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok && gerr.Code == http.StatusConflict {
		err = nil
	}
	if err != nil {
		t.Fatalf("create gs://%s: %v", emulatorBucket, err)
	}
}

func ensureS3Bucket(t *testing.T) {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(t.Context())
	must(t, err)
	c := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(os.Getenv(S3EndpointEnv))
		o.UsePathStyle = true
	})
	_, err = c.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(emulatorBucket)})
	if _, ok := errors.AsType[*types.BucketAlreadyOwnedByYou](err); ok {
		err = nil
	}
	if err != nil {
		t.Fatalf("create s3://%s: %v", emulatorBucket, err)
	}
}

// freshPrefix is a prefix no other test or run has used, so each bucket starts empty.
func freshPrefix(t *testing.T) string {
	return "e2e-" + randomHex(8) + "/" + strings.NewReplacer("/", "-", " ", "_").Replace(t.Name()) + "/"
}

// landThenFail passes every request to base. The first conditional create after Arm reaches
// the server, then its response is replaced with a 503, as if it was lost on the way back:
// the SDK sees a retryable failure of a create that landed.
type landThenFail struct {
	base     http.RoundTripper
	isCreate func(*http.Request) bool
	armed    atomic.Bool
	creates  atomic.Int32
	landed   atomic.Int32 // the status the swallowed response had
}

func (l *landThenFail) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.base.RoundTrip(req)
	if err != nil || !l.isCreate(req) {
		return resp, err
	}
	l.creates.Add(1)
	if !l.armed.CompareAndSwap(true, false) {
		return resp, nil
	}
	l.landed.Store(int32(resp.StatusCode))
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	body := "injected: the response was lost"
	return &http.Response{
		Status:        "503 Service Unavailable",
		StatusCode:    http.StatusServiceUnavailable,
		Proto:         resp.Proto,
		ProtoMajor:    resp.ProtoMajor,
		ProtoMinor:    resp.ProtoMinor,
		Header:        http.Header{"Content-Type": {"text/plain"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// TestEmulators runs, on each emulator: the bucket conformance, a race of conditional creates
// (one winner), the SDK's retry of a create that landed, and storetest.Run through the object
// store.
func TestEmulators(t *testing.T) {
	for _, e := range emulators(t) {
		t.Run(e.scheme, func(t *testing.T) {
			t.Run("bucket", func(t *testing.T) {
				// Through ParseURL, as serve opens it.
				b, err := ParseURL(t.Context(), e.scheme+"://"+emulatorBucket+"/"+strings.TrimSuffix(freshPrefix(t), "/"))
				must(t, err)
				if g, ok := b.(*GCSBucket); ok {
					t.Cleanup(func() { _ = g.Close() })
				}
				testBucket(t, b)
			})
			t.Run("create race", func(t *testing.T) { testCreateRace(t, e.open(t, freshPrefix(t), nil)) })
			t.Run("sdk retry", func(t *testing.T) { testSDKRetry(t, e) })
			t.Run("store", func(t *testing.T) {
				storetest.Run(t, func(o store.Options) storetest.Subject {
					return conformanceStore(t, e.open(t, freshPrefix(t), nil), o)
				})
			})
		})
	}
}

// testCreateRace: conditional creates of one name at once: exactly one wins, the others get
// ErrExists, and the object holds the winner's bytes.
func testCreateRace(t *testing.T, b Bucket) {
	const n = 8
	ctx := t.Context()
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = b.Create(ctx, "log/race", fmt.Appendf(nil, "writer %d", i)) })
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner >= 0:
			t.Fatalf("writers %d and %d both created log/race", winner, i)
		case err == nil:
			winner = i
		case !errors.Is(err, ErrExists):
			t.Fatalf("writer %d: %v, want nil or ErrExists", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no writer created log/race")
	}
	got, err := b.Get(ctx, "log/race")
	must(t, err)
	if want := fmt.Sprintf("writer %d", winner); string(got) != want {
		t.Fatalf("log/race = %q, want the winner's %q", got, want)
	}
}

// testSDKRetry: a conditional create that landed but whose response was lost (a 503) is
// retried by the SDK, and the retry answers ErrExists. Through the store, the write's own
// nonce is found, so it is committed once: a BudgetCounter charged once, not ErrConflict.
func testSDKRetry(t *testing.T, e emulator) {
	ctx := t.Context()
	newTransport := func() *landThenFail {
		return &landThenFail{base: http.DefaultTransport.(*http.Transport).Clone(), isCreate: e.isCreate}
	}

	// The bucket alone.
	lt := newTransport()
	b := e.open(t, freshPrefix(t), &http.Client{Transport: lt})
	lt.armed.Store(true)
	err := b.Create(ctx, "log/1", []byte("one"))
	if lt.landed.Load() != http.StatusOK {
		t.Fatalf("the first create answered %d before it was swallowed, want 200", lt.landed.Load())
	}
	if n := lt.creates.Load(); n != 2 {
		t.Fatalf("%d create requests, want 2: the SDK didn't retry the 503 (Create = %v)", n, err)
	}
	if !errors.Is(err, ErrExists) {
		t.Fatalf("Create after a retry of a create that landed = %v, want ErrExists", err)
	}
	if got, err := b.Get(ctx, "log/1"); err != nil || string(got) != "one" {
		t.Fatalf("Get = %q, %v", got, err)
	}

	// Through the store.
	lt = newTransport()
	prefix := freshPrefix(t)
	opts, _ := testOpts()
	s := mustOpen(t, e.open(t, prefix, &http.Client{Transport: lt}), opts, WithWriterID("A"), withoutBackground())
	c := newCounter("budget.global.all.2026-01-01")
	must(t, s.Create(ctx, c))
	lt.armed.Store(true)
	before := lt.creates.Load()
	c.Status.Runs++
	if err := s.Update(ctx, c); err != nil {
		t.Fatalf("Update whose create the SDK retried = %v, want committed", err)
	}
	if n := lt.creates.Load() - before; n != 2 {
		t.Fatalf("%d create requests for the update, want 2 (the lost one and the SDK's retry)", n)
	}
	// A second store on the bucket sees the counter charged once, at the update's version.
	other := mustOpen(t, e.open(t, prefix, nil), opts, WithWriterID("B"), withoutBackground())
	var got v1alpha1.BudgetCounter
	must(t, other.Get(ctx, store.KeyOf(c), &got))
	if got.Status.Runs != 1 || got.Metadata.ResourceVersion != c.Metadata.ResourceVersion {
		t.Fatalf("counter %+v at %s, want 1 run at %s", got.Status, got.Metadata.ResourceVersion, c.Metadata.ResourceVersion)
	}
	var entries int
	must(t, e.open(t, prefix, nil).List(ctx, logPrefix, "", func(ObjectInfo) error { entries++; return nil }))
	if entries != 2 {
		t.Fatalf("%d log entries, want 2 (the create and the update, once)", entries)
	}
}
