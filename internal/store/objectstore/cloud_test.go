package objectstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/ContainerSolutions/factory/internal/clock"
)

// TestParseCloudURL: gs:// and s3:// take a bucket and an optional prefix, and refuse
// anything that could carry a credential or isn't a plain path.
func TestParseCloudURL(t *testing.T) {
	for _, tc := range []struct {
		url          string
		bucket, pref string
		bad          string // a substring of the error, "" when accepted
	}{
		{url: "gs://b", bucket: "b"},
		{url: "gs://b/", bucket: "b"},
		{url: "gs://my-bucket.x_1/factory", bucket: "my-bucket.x_1", pref: "factory/"},
		{url: "s3://b/a/b/", bucket: "b", pref: "a/b/"},
		{url: "gs://", bad: "no bucket"},
		{url: "gs:///p", bad: "no bucket"},
		{url: "gs://Bucket/p", bad: "bucket name may only"},
		{url: "s3://key:secret@b/p", bad: "has credentials"},
		{url: "s3://key@b/p", bad: "has credentials"},
		{url: "s3://key:/secret@b/p", bad: "has credentials"},
		{url: "gs://b/p@x", bad: "has credentials"},
		{url: "gs://b:443/p", bad: "port"},
		{url: "gs://b:/p", bad: "port"},
		{url: "gs://b/a b", bad: "percent-escaped"},
		{url: "gs://b/a+b", bad: "prefix may only"},
		{url: "gs://b/a:b", bad: "prefix may only"},
		{url: "gs://b/My_Prefix-1.x/y", bucket: "b", pref: "My_Prefix-1.x/y/"},
		{url: "gs://b/p?x=1", bad: "query"},
		{url: "gs://b/p?", bad: "query"},
		{url: "gs://b/p#f", bad: "fragment"},
		{url: "gs:b/p", bad: "not a bucket URL"},
		{url: "gs://b//p", bad: "segment"},
		{url: "gs://b/a/./p", bad: "segment"},
		{url: "gs://b/a/../p", bad: "segment"},
		{url: "gs://b/a%2Fb", bad: "percent-escaped"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			u, err := url.Parse(tc.url)
			must(t, err)
			loc, err := parseCloudURL(u)
			if tc.bad != "" {
				if err == nil || !strings.Contains(err.Error(), tc.bad) {
					t.Fatalf("parseCloudURL = %+v, %v; want an error with %q", loc, err, tc.bad)
				}
				// Nothing of the URL but its scheme: not the secret, the bucket or the prefix.
				for _, part := range []string{"secret", "key", "Bucket", "443", "a b", "a+b", "a:b", "x=1", "#f", "/./", "/../"} {
					if strings.Contains(tc.url, part) && strings.Contains(err.Error(), part) {
						t.Fatalf("the error repeats %q from the URL: %v", part, err)
					}
				}
				return
			}
			if err != nil || loc.bucket != tc.bucket || loc.prefix != tc.pref {
				t.Fatalf("parseCloudURL = %+v, %v; want bucket %q prefix %q", loc, err, tc.bucket, tc.pref)
			}
		})
	}
}

// secretPiece returns a piece of secret, 3 characters or more, that msg holds, or "".
func secretPiece(msg, secret string) string {
	for n := 3; n <= len(secret); n++ {
		for i := 0; i+n <= len(secret); i++ {
			if sub := secret[i : i+n]; strings.Contains(msg, sub) {
				return sub
			}
		}
	}
	return ""
}

// TestCloudURLSecrets: a gs:// or s3:// URL with a credential in it is refused, alone or
// with anything else wrong with it, and neither parseCloudURL's error nor ParseURL's holds
// any 3 characters of the secret. Secrets with '/', '?', '#' or '"' land outside the
// userinfo (the path, the query, the fragment), or don't parse. Every case is refused
// before any network or SDK config is touched.
func TestCloudURLSecrets(t *testing.T) {
	type tc struct{ url, secret string }
	var cases []tc
	for _, scheme := range []string{"gs", "s3"} {
		for _, c := range []tc{
			{"KEY:hunter2@bucket/p", "hunter2"},         // userinfo alone
			{"KEY:hunter2@bucket", "hunter2"},           // no prefix
			{"KEY:hunter2@/p", "hunter2"},               // no bucket
			{"KEY:hunter2@Bucket/p", "hunter2"},         // a bad bucket name
			{"KEY:hunter2@bucket:9000/p", "hunter2"},    // a port
			{"KEY:hunter2@bucket/p?x=1", "hunter2"},     // a query
			{"KEY:hunter2@bucket/p#f", "hunter2"},       // a fragment
			{"KEY:hunter2@bucket/a//b", "hunter2"},      // an empty segment
			{"KEY:hunter2@bucket/a/../b", "hunter2"},    // a '..' segment
			{"KEY:hunter2@bucket/a%2Fb", "hunter2"},     // an escaped prefix
			{"hunter2@bucket/p", "hunter2"},             // the secret as the user
			{"KEY:hunter2%zz@bucket/p", "hunter2"},      // a bad escape (doesn't parse)
			{"KEY:hunter2@bucket:port/p", "hunter2"},    // a bad port (doesn't parse)
			{"KEY:hun%20ter2@bucket/p", "hun ter2"},     // an escaped space in the secret
			{"KEY:hunter2@bu%20cket/p", "hunter2"},      // an escaped host
			{"KEY:/hunter2XYZ@bucket/p", "/hunter2XYZ"}, // a leading '/': the secret is the path
			{"u:/hunter2XYZ@b/p", "/hunter2XYZ"},        // and a short bucket after it
			{"KEY:hun/ter2XYZ@bucket/p", "hun/ter2XYZ"}, // a '/' inside
			{"KEY:hun?ter2XYZ@bucket/p", "hun?ter2XYZ"}, // a '?': the tail is the query
			{"KEY:hun#ter2XYZ@bucket/p", "hun#ter2XYZ"}, // a '#': the tail is the fragment
			{`KEY:hun"ter2XYZ@bucket/p`, `hun"ter2XYZ`}, // a '"' (doesn't parse)
			{`KEY:"hunter2XYZ"@bucket/p`, `"hunter2XYZ"`},
			{"KEY:a/b?c#hunter2XYZ@bucket/p", "a/b?c#hunter2XYZ"}, // all of them
			// The round-2 review's cases: a port-like prefix before the '/', a secret
			// after '?' or '#', and a credential in the query or the fragment.
			{"KEY:/hunter2@bucket/p", "/hunter2"},
			{"KEY:12/hunter2@bucket/p", "12/hunter2"},
			{"KEY:?hunter2@bucket/p", "?hunter2"},
			{"KEY:#hunter2@bucket/p", "#hunter2"},
			{"bucket/p?AWS_SECRET_ACCESS_KEY=hunter2", "hunter2"},
			{"bucket/p#hunter2", "hunter2"},
			{"bucket?CREDENTIALS=hunter2", "hunter2"},
		} {
			cases = append(cases, tc{scheme + "://" + c.url, c.secret})
		}
	}
	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			if u, err := url.Parse(c.url); err == nil {
				loc, err := parseCloudURL(u)
				if err == nil {
					t.Fatalf("parseCloudURL(%q) = %+v, want refused", c.url, loc)
				}
				if p := secretPiece(err.Error(), c.secret); p != "" {
					t.Fatalf("parseCloudURL's error holds %q of the secret: %v", p, err)
				}
			}
			b, err := ParseURL(t.Context(), c.url)
			if err == nil {
				t.Fatalf("ParseURL(%q) = %v, want refused", c.url, b)
			}
			if p := secretPiece(err.Error(), c.secret); p != "" {
				t.Fatalf("ParseURL's error holds %q of the secret: %v", p, err)
			}
		})
	}
}

// s3TestEnv points the AWS SDK's default chain at test values only: a fixed key pair and
// region, no shared config files, no instance metadata, and FACTORY_S3_ENDPOINT = endpoint.
func s3TestEnv(t *testing.T, endpoint string) {
	t.Helper()
	none := filepath.Join(t.TempDir(), "none")
	for k, v := range map[string]string{
		"AWS_ACCESS_KEY_ID":           "factorytest",
		"AWS_SECRET_ACCESS_KEY":       "factorytest-not-a-secret",
		"AWS_SESSION_TOKEN":           "",
		"AWS_PROFILE":                 "",
		"AWS_REGION":                  "us-east-1",
		"AWS_CONFIG_FILE":             none,
		"AWS_SHARED_CREDENTIALS_FILE": none,
		"AWS_EC2_METADATA_DISABLED":   "true",
		"AWS_ENDPOINT_URL":            "",
		"AWS_ENDPOINT_URL_S3":         "",
		S3EndpointEnv:                 endpoint,
	} {
		t.Setenv(k, v)
	}
}

// TestParseURLClouds: ParseURL opens gs:// and s3:// URLs. The GCS bucket lists its prefix
// once at open against a canned server, so a missing bucket fails there; the client outlives
// the open's context. The S3 config is read from the environment: S3 needs a region, and
// FACTORY_S3_ENDPOINT must be https://, or http:// on a loopback host.
func TestParseURLClouds(t *testing.T) {
	bucketMissing := `{"error":{"code":404,"message":"The specified bucket does not exist.","errors":[{"message":"The specified bucket does not exist.","domain":"global","reason":"notFound"}]}}`
	missing := false
	srv, c := newCannedServer(t, func(string, int) (int, string) {
		if missing {
			return http.StatusNotFound, bucketMissing
		}
		return http.StatusOK, `{"kind":"storage#objects"}`
	})
	t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
	ctx, cancel := context.WithCancel(t.Context())
	b, err := ParseURL(ctx, "gs://bucket/factory/state")
	must(t, err)
	if g, ok := b.(*GCSBucket); !ok || g.String() != "gs://bucket/factory/state/" {
		t.Fatalf("ParseURL(gs://) = %#v", b)
	}
	if n := c.count(http.MethodGet); n != 1 {
		t.Fatalf("%d GETs at open, want 1 (the list)", n)
	}
	cancel() // the open's context ends; the bucket goes on
	if err := b.List(t.Context(), "log/", "", func(ObjectInfo) error { return nil }); err != nil {
		t.Fatalf("List after the open's context ended = %v", err)
	}
	must(t, b.(*GCSBucket).Close())
	missing = true
	_, err = ParseURL(t.Context(), "gs://bucket/factory/state")
	if err == nil || errors.Is(err, ErrNotExist) || !strings.Contains(err.Error(), "bucket does not exist") {
		t.Fatalf("ParseURL on a missing bucket = %v, want an error saying so, not ErrNotExist", err)
	}
	if _, err := ParseURL(t.Context(), "gs://b/p?project=x"); err == nil {
		t.Fatal("gs:// with a query was accepted")
	}

	s3TestEnv(t, "")
	b, err = ParseURL(t.Context(), "s3://bucket/factory")
	must(t, err)
	if s, ok := b.(*S3Bucket); !ok || s.String() != "s3://bucket/factory/" {
		t.Fatalf("ParseURL(s3://) = %#v", b)
	}
	for _, ep := range []string{
		"https://minio.test", "https://minio.test/", "https://10.0.0.5:9000",
		"http://127.0.0.1:9000", "http://localhost:9000", "http://LOCALHOST", "http://[::1]:9000",
	} {
		t.Setenv(S3EndpointEnv, ep)
		if _, err := ParseURL(t.Context(), "s3://bucket"); err != nil {
			t.Fatalf("%s=%s: %v", S3EndpointEnv, ep, err)
		}
	}
	for _, tc := range []struct{ ep, why string }{
		{"http://minio.test:9000", "only for a loopback host"},
		{"http://10.0.0.5:9000", "only for a loopback host"},
		{"http://127.0.0.2:9000", "only for a loopback host"},
		{"http://localhost.example.com", "only for a loopback host"},
		{"https://u:hunter2@minio.test", "has credentials"},
		{"http://u:hunter2@localhost", "has credentials"},
		{"http://u:hunter2@h:x:y", "not a URL"},
		{"https://hunter2@minio.test", "has credentials"},
		{"https://u:/hunter2@minio.test", "has credentials"},
		{"https://u:x?hunter2@minio.test", "not a URL"},
		{"https://u:x#hunter2@minio.test", "not a URL"},
		{"https://minio.test#hunter2@x", "has credentials"},
		{"https://minio.test/hunter2@x", "has credentials"},
		{`https://u:x"hunter2@minio.test`, "not a URL"},
		{"https://u:hun/ter2@minio.test", "not a URL"},
		{"https://u:hun?ter2@minio.test", "not a URL"},
		{"https://u:hun#ter2@minio.test", "not a URL"},
		{"https://KEY:/hunter2@minio.example:9000", "has credentials"},
		{"https://KEY:12/hunter2@minio.test", "has credentials"},
		{"https://minio.test?AWS_SECRET_ACCESS_KEY=hunter2", "a path, a query or a fragment"},
		{"https://minio.test#hunter2", "a path, a query or a fragment"},
		{"minio:9000", "scheme is not http or https"},
		{"ftp://h", "scheme is not http or https"},
		{"https://", "no host"},
		{"https://h/somedir", "a path, a query or a fragment"},
		{"https://h?x=1", "a path, a query or a fragment"},
		{"https://h?", "a path, a query or a fragment"},
		{"https://h#f", "a path, a query or a fragment"},
	} {
		t.Setenv(S3EndpointEnv, tc.ep)
		_, err := ParseURL(t.Context(), "s3://bucket")
		if err == nil || !strings.Contains(err.Error(), S3EndpointEnv) || !strings.Contains(err.Error(), tc.why) {
			t.Fatalf("%s=%s = %v, want refused (%s)", S3EndpointEnv, tc.ep, err, tc.why)
		}
		// Nothing of the value is printed: no 3 characters of the secret, and not its host.
		if p := secretPiece(err.Error(), "hunter2"); p != "" {
			t.Fatalf("the error holds %q of the endpoint's secret: %v", p, err)
		}
		for _, part := range []string{"minio.test", "minio.example", "10.0.0.5", "127.0.0.2", "example.com", "somedir", "x=1", "AWS_SECRET"} {
			if strings.Contains(err.Error(), part) {
				t.Fatalf("the error repeats %q from the endpoint: %v", part, err)
			}
		}
	}
	t.Setenv(S3EndpointEnv, "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	if _, err := ParseURL(t.Context(), "s3://bucket"); err == nil || !strings.Contains(err.Error(), "AWS_REGION") {
		t.Fatalf("s3:// with no region = %v, want an error naming AWS_REGION", err)
	}
}

// cannedServer answers every request with the status and body reply gives for its method,
// and counts the requests per method.
type cannedServer struct {
	mu    sync.Mutex
	calls map[string]int
	reply func(method string, n int) (int, string)
}

func newCannedServer(t *testing.T, reply func(method string, n int) (int, string)) (*httptest.Server, *cannedServer) {
	c := &cannedServer{calls: map[string]int{}, reply: reply}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.calls[r.Method]++
		n := c.calls[r.Method]
		c.mu.Unlock()
		status, body := c.reply(r.Method, n)
		if body != "" {
			w.Header().Set("Content-Type", "application/xml")
			if strings.HasPrefix(body, "{") {
				w.Header().Set("Content-Type", "application/json")
			}
		}
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func (c *cannedServer) count(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[method]
}

// sentinel names the Bucket error err is, for the tables.
func sentinel(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, ErrNotExist):
		return "ErrNotExist"
	case errors.Is(err, ErrExists):
		return "ErrExists"
	}
	return "other"
}

// gcsServer answers a GCS list (GET …/o) with list, and every other request with object,
// and counts the lists.
func gcsServer(t *testing.T, object, list func() (int, string)) (*httptest.Server, *atomic.Int32) {
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body := object()
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/o") {
			lists.Add(1)
			status, body = list()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &lists
}

// TestGCSErrorMapping: a 412 on a create is ErrExists. An object 404 on a get or a delete
// is ErrNotExist only once a one-object list of the prefix succeeds; a missing bucket's 404
// is not ErrNotExist, either by its message (no list) or, when GCS words it otherwise, by the
// list's own 404. Other statuses map to neither. A mapped error keeps the SDK's in its chain.
// 4xx are not retried, so each case is a fixed number of requests against a canned server.
func TestGCSErrorMapping(t *testing.T) {
	gcsError := func(code int, msg string) string {
		return fmt.Sprintf(`{"error":{"code":%d,"message":%q,"errors":[{"message":%q,"domain":"global","reason":"notFound"}]}}`, code, msg, msg)
	}
	reply := func(code int, msg string) func() (int, string) {
		return func() (int, string) { return code, gcsError(code, msg) }
	}
	listOK := func() (int, string) { return http.StatusOK, `{"kind":"storage#objects"}` }
	const (
		noObject = "No such object: b/p/log/1"
		noBucket = "The specified bucket does not exist."
		reworded = "Not found." // a message gcsBucketMissing doesn't know
	)
	get := func(b Bucket) error { _, err := b.Get(t.Context(), "log/1"); return err }
	create := func(b Bucket) error { return b.Create(t.Context(), "log/1", []byte("x")) }
	del := func(b Bucket) error { return b.Delete(t.Context(), "log/1") }
	list := func(b Bucket) error { return b.List(t.Context(), "log/", "", func(ObjectInfo) error { return nil }) }
	for _, tc := range []struct {
		name         string
		op           func(b Bucket) error
		object, list func() (int, string)
		want         string
		lists        int32
	}{
		{"get 404, list 200", get, reply(404, noObject), listOK, "ErrNotExist", 1},
		{"get 404 (no bucket)", get, reply(404, noBucket), listOK, "no bucket", 0},
		{"get 404 reworded, list 404", get, reply(404, reworded), reply(404, reworded), "no bucket", 1},
		{"get 404, list 403", get, reply(404, noObject), reply(403, "canned"), "other", 1},
		{"get 403", get, reply(403, "canned"), listOK, "other", 0},
		{"create 412", create, reply(412, "conditionNotMet"), listOK, "ErrExists", 0},
		{"create 403", create, reply(403, "canned"), listOK, "other", 0},
		{"create 404 (no bucket)", create, reply(404, noBucket), listOK, "no bucket", 0},
		{"delete 404, list 200", del, reply(404, noObject), listOK, "ErrNotExist", 1},
		{"delete 404 (no bucket)", del, reply(404, noBucket), listOK, "no bucket", 0},
		{"delete 404 reworded, list 404", del, reply(404, reworded), reply(404, reworded), "no bucket", 1},
		{"delete 403", del, reply(403, "canned"), listOK, "other", 0},
		{"list 404 (no bucket)", list, listOK, reply(404, reworded), "no bucket", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, lists := gcsServer(t, tc.object, tc.list)
			t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
			b, err := newGCSBucket(t.Context(), cloudLocation{bucket: "b", prefix: "p/"}, nil)
			must(t, err)
			defer func() { _ = b.Close() }()
			err = tc.op(b)
			want := tc.want
			if want == "no bucket" {
				want = "other"
				if err == nil || !strings.Contains(err.Error(), "the bucket does not exist") {
					t.Errorf("%s = %v, want it to say the bucket does not exist", tc.name, err)
				}
			}
			if got := sentinel(err); got != want {
				t.Fatalf("%s = %s (%v), want %s", tc.name, got, err, want)
			}
			if want == "ErrNotExist" || want == "ErrExists" {
				if _, ok := errors.AsType[*googleapi.Error](err); !ok {
					t.Fatalf("%s = %v: the SDK's error is not in the chain", tc.name, err)
				}
			}
			if n := lists.Load(); n != tc.lists {
				t.Fatalf("%s: %d lists, want %d", tc.name, n, tc.lists)
			}
		})
	}
}

// TestGCSConfirmWindow: a not-found Get confirms the bucket with a list at most once per
// window (on an injected clock): not within it after a proof (a confirmed 404 or a
// successful request), once after it, and a bucket gone after the window is reported
// missing, not ErrNotExist. Parallel not-found Gets send one list between them.
func TestGCSConfirmWindow(t *testing.T) {
	gcsError := func(code int, msg string) string {
		return fmt.Sprintf(`{"error":{"code":%d,"message":%q,"errors":[{"message":%q,"domain":"global","reason":"notFound"}]}}`, code, msg, msg)
	}
	var objectFound, bucketGone atomic.Bool
	object := func() (int, string) {
		switch {
		case bucketGone.Load():
			return 404, gcsError(404, "Not found.") // reworded: the message match misses it
		case objectFound.Load():
			return 200, "data"
		}
		return 404, gcsError(404, "No such object: b/p/log/1")
	}
	list := func() (int, string) {
		if bucketGone.Load() {
			return 404, gcsError(404, "Not found.")
		}
		time.Sleep(20 * time.Millisecond) // long enough for parallel callers to pile up
		return 200, `{"kind":"storage#objects"}`
	}
	srv, lists := gcsServer(t, object, list)
	t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
	clk := clock.NewFake(epoch)
	const window = time.Minute
	b, err := newGCSBucket(t.Context(), cloudLocation{bucket: "b", prefix: "p/"}, nil, withGCSClock(clk), withGCSConfirmEvery(window))
	must(t, err)
	defer func() { _ = b.Close() }()

	get := func(want string, wantLists int32) {
		t.Helper()
		_, err := b.Get(t.Context(), "log/1")
		got := sentinel(err)
		if want == "no bucket" {
			if got != "other" || !strings.Contains(err.Error(), "the bucket does not exist") {
				t.Fatalf("Get = %v, want the bucket reported missing", err)
			}
		} else if got != want {
			t.Fatalf("Get = %s (%v), want %s", got, err, want)
		}
		if n := lists.Load(); n != wantLists {
			t.Fatalf("%d lists so far, want %d", n, wantLists)
		}
	}
	get("ErrNotExist", 1) // never proven: one list
	get("ErrNotExist", 1) // the confirmed 404 is the proof
	clk.Advance(window - time.Second)
	get("ErrNotExist", 1) // still within the window
	clk.Advance(2 * time.Second)
	get("ErrNotExist", 2) // past it: one list
	clk.Advance(2 * window)
	objectFound.Store(true)
	get("nil", 2) // a successful Get proves the bucket
	objectFound.Store(false)
	get("ErrNotExist", 2) // so the next 404 needs no list
	clk.Advance(2 * window)
	bucketGone.Store(true)
	get("no bucket", 3) // gone after the window: the list's 404 says so
	bucketGone.Store(false)

	// Parallel not-found Gets past the window: one list between them.
	clk.Advance(2 * window)
	before := lists.Load()
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Go(func() { _, errs[i] = b.Get(t.Context(), "log/1") })
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, ErrNotExist) {
			t.Fatalf("parallel Get %d = %v, want ErrNotExist", i, err)
		}
	}
	if n := lists.Load() - before; n != 1 {
		t.Fatalf("parallel not-found Gets sent %d lists, want 1", n)
	}
}

// TestS3ErrorMapping: NoSuchKey on a GET and a 404 on the delete's HEAD are ErrNotExist
// (NoSuchBucket is not), a 412 on a create is ErrExists, a create S3 answers 409
// ConditionalRequestConflict is sent again (and stays an unknown outcome when the conflict
// persists), and other errors map to neither.
func TestS3ErrorMapping(t *testing.T) {
	s3Error := func(code string) string {
		return `<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>canned</Message></Error>`
	}
	get := func(b Bucket) error { _, err := b.Get(t.Context(), "log/1"); return err }
	create := func(b Bucket) error { return b.Create(t.Context(), "log/1", []byte("x")) }
	del := func(b Bucket) error { return b.Delete(t.Context(), "log/1") }
	for _, tc := range []struct {
		name  string
		op    func(b Bucket) error
		reply func(method string, n int) (int, string)
		want  string
		calls map[string]int
	}{
		{"get NoSuchKey", get, func(string, int) (int, string) { return 404, s3Error("NoSuchKey") }, "ErrNotExist", map[string]int{"GET": 1}},
		{"get NoSuchBucket", get, func(string, int) (int, string) { return 404, s3Error("NoSuchBucket") }, "other", map[string]int{"GET": 1}},
		{"get AccessDenied", get, func(string, int) (int, string) { return 403, s3Error("AccessDenied") }, "other", map[string]int{"GET": 1}},
		{"create PreconditionFailed", create, func(string, int) (int, string) { return 412, s3Error("PreconditionFailed") }, "ErrExists", map[string]int{"PUT": 1}},
		{"create AccessDenied", create, func(string, int) (int, string) { return 403, s3Error("AccessDenied") }, "other", map[string]int{"PUT": 1}},
		{"create conflict, then created", create, func(_ string, n int) (int, string) {
			if n == 1 {
				return 409, s3Error("ConditionalRequestConflict")
			}
			return 200, ""
		}, "nil", map[string]int{"PUT": 2}},
		{"create conflict, then taken", create, func(_ string, n int) (int, string) {
			if n == 1 {
				return 409, s3Error("ConditionalRequestConflict")
			}
			return 412, s3Error("PreconditionFailed")
		}, "ErrExists", map[string]int{"PUT": 2}},
		{"create conflict persists", create, func(string, int) (int, string) { return 409, s3Error("ConditionalRequestConflict") }, "other", map[string]int{"PUT": 1 + s3ConflictRetries}},
		{"create", create, func(string, int) (int, string) { return http.StatusOK, "" }, "nil", map[string]int{"PUT": 1}},
		{"delete missing", del, func(string, int) (int, string) { return 404, "" }, "ErrNotExist", map[string]int{"HEAD": 1}},
		{"delete present", del, func(m string, _ int) (int, string) {
			if m == http.MethodDelete {
				return 204, ""
			}
			return 200, ""
		}, "nil", map[string]int{"HEAD": 1, "DELETE": 1}},
		{"delete AccessDenied", del, func(string, int) (int, string) { return 403, "" }, "other", map[string]int{"HEAD": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, c := newCannedServer(t, tc.reply)
			s3TestEnv(t, srv.URL)
			b, err := newS3Bucket(t.Context(), cloudLocation{bucket: "b", prefix: "p/"}, nil)
			must(t, err)
			if got := sentinel(tc.op(b)); got != tc.want {
				t.Fatalf("%s = %s, want %s", tc.name, got, tc.want)
			}
			for m, n := range tc.calls {
				if got := c.count(m); got != n {
					t.Errorf("%d %s requests, want %d", got, m, n)
				}
			}
			for _, m := range []string{"GET", "PUT", "HEAD", "DELETE", "POST"} {
				if _, want := tc.calls[m]; !want && c.count(m) != 0 {
					t.Errorf("%d unexpected %s requests", c.count(m), m)
				}
			}
		})
	}
}
