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
	"testing"

	"google.golang.org/api/googleapi"
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
		{url: "gs://Bucket/p", bad: "only a-z"},
		{url: "s3://key:secret@b/p", bad: "credentials in the URL"},
		{url: "s3://key@b/p", bad: "credentials in the URL"},
		{url: "gs://b:443/p", bad: "port"},
		{url: "gs://b/p?x=1", bad: "query"},
		{url: "gs://b/p?", bad: "query"},
		{url: "gs://b/p#f", bad: "fragment"},
		{url: "gs:b/p", bad: "not a bucket URL"},
		{url: "gs://b//p", bad: "segment"},
		{url: "gs://b/a/./p", bad: "segment"},
		{url: "gs://b/a/../p", bad: "segment"},
		{url: "gs://b/a%2Fb", bad: "escaped"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			u, err := url.Parse(tc.url)
			must(t, err)
			loc, err := parseCloudURL(u)
			if tc.bad != "" {
				if err == nil || !strings.Contains(err.Error(), tc.bad) {
					t.Fatalf("parseCloudURL = %+v, %v; want an error with %q", loc, err, tc.bad)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatalf("the error shows the URL's password: %v", err)
				}
				return
			}
			if err != nil || loc.bucket != tc.bucket || loc.prefix != tc.pref {
				t.Fatalf("parseCloudURL = %+v, %v; want bucket %q prefix %q", loc, err, tc.bucket, tc.pref)
			}
		})
	}
}

// TestCloudURLSecrets: a gs:// or s3:// URL with userinfo is refused, alone or with anything
// else wrong with it, and neither parseCloudURL's error nor ParseURL's ever repeats the
// secret. Every case is refused before any network or SDK config is touched.
func TestCloudURLSecrets(t *testing.T) {
	const secret = "hunter2"
	var urls []string
	for _, scheme := range []string{"gs", "s3"} {
		for _, rest := range []string{
			"KEY:hunter2@bucket/p",      // userinfo alone
			"KEY:hunter2@bucket",        // no prefix
			"KEY:hunter2@/p",            // no bucket
			"KEY:hunter2@Bucket/p",      // a bad bucket name
			"KEY:hunter2@bucket:9000/p", // a port
			"KEY:hunter2@bucket/p?x=1",  // a query
			"KEY:hunter2@bucket/p#f",    // a fragment
			"KEY:hunter2@bucket/a//b",   // an empty segment
			"KEY:hunter2@bucket/a/../b", // a '..' segment
			"KEY:hunter2@bucket/a%2Fb",  // an escaped prefix
			"hunter2@bucket/p",          // the secret as the user
			"KEY:hunter2%zz@bucket/p",   // a bad escape (doesn't parse)
			"KEY:hunter2@bucket:port/p", // a bad port (doesn't parse)
			"KEY:hun%20ter2@bucket/p",   // an escaped space in the secret
			"KEY:hunter2@bu%20cket/p",   // an escaped host
		} {
			urls = append(urls, scheme+"://"+rest)
		}
	}
	leaks := func(err error) bool {
		msg := err.Error()
		return strings.Contains(msg, secret) || strings.Contains(msg, "hun%20ter2") || strings.Contains(msg, "hun ter2")
	}
	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			if u, err := url.Parse(raw); err == nil {
				loc, err := parseCloudURL(u)
				if err == nil {
					t.Fatalf("parseCloudURL(%q) = %+v, want refused", raw, loc)
				}
				if leaks(err) {
					t.Fatalf("parseCloudURL's error repeats the secret: %v", err)
				}
			}
			b, err := ParseURL(t.Context(), raw)
			if err == nil {
				t.Fatalf("ParseURL(%q) = %v, want refused", raw, b)
			}
			if leaks(err) {
				t.Fatalf("ParseURL's error repeats the secret: %v", err)
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
		{"https://u:hunter2@minio.test", "credentials in the URL"},
		{"http://u:hunter2@localhost", "credentials in the URL"},
		{"http://u:hunter2@h:x:y", "not a URL"},
		{"https://hunter2@minio.test", "credentials in the URL"},
		{"minio:9000", "want https://"},
		{"ftp://h", "want https://"},
		{"https://h/path", "want https://"},
		{"https://h?x=1", "want https://"},
		{"https://h?", "want https://"},
		{"https://h#f", "want https://"},
	} {
		t.Setenv(S3EndpointEnv, tc.ep)
		_, err := ParseURL(t.Context(), "s3://bucket")
		if err == nil || !strings.Contains(err.Error(), S3EndpointEnv) || !strings.Contains(err.Error(), tc.why) {
			t.Fatalf("%s=%s = %v, want refused (%s)", S3EndpointEnv, tc.ep, err, tc.why)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("the error shows the endpoint's password: %v", err)
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

// TestGCSErrorMapping: a missing object's 404 on a read or a delete is ErrNotExist, a 412
// on a create is ErrExists, and a missing bucket's 404 (GCS's real body) and other statuses
// map to neither. A mapped error keeps the SDK's in its chain. 4xx are not retried, so each
// case is one request against a canned server.
func TestGCSErrorMapping(t *testing.T) {
	gcsError := func(code int, msg string) string {
		return fmt.Sprintf(`{"error":{"code":%d,"message":%q,"errors":[{"message":%q,"domain":"global","reason":"notFound"}]}}`, code, msg, msg)
	}
	const (
		noObject = "No such object: b/p/log/1"
		noBucket = "The specified bucket does not exist."
	)
	get := func(b Bucket) error { _, err := b.Get(t.Context(), "log/1"); return err }
	create := func(b Bucket) error { return b.Create(t.Context(), "log/1", []byte("x")) }
	del := func(b Bucket) error { return b.Delete(t.Context(), "log/1") }
	list := func(b Bucket) error { return b.List(t.Context(), "log/", "", func(ObjectInfo) error { return nil }) }
	for _, tc := range []struct {
		name   string
		op     func(b Bucket) error
		status int
		msg    string
		want   string
	}{
		{"get 404", get, 404, noObject, "ErrNotExist"},
		{"get 404 (no bucket)", get, 404, noBucket, "no bucket"},
		{"get 403", get, 403, "canned", "other"},
		{"create 412", create, 412, "conditionNotMet", "ErrExists"},
		{"create 403", create, 403, "canned", "other"},
		{"create 404 (no bucket)", create, 404, noBucket, "no bucket"},
		{"delete 404", del, 404, noObject, "ErrNotExist"},
		{"delete 404 (no bucket)", del, 404, noBucket, "no bucket"},
		{"delete 403", del, 403, "canned", "other"},
		{"list 404 (no bucket)", list, 404, noBucket, "no bucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newCannedServer(t, func(string, int) (int, string) { return tc.status, gcsError(tc.status, tc.msg) })
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
			if want != "other" {
				var gerr *googleapi.Error
				if !errors.As(err, &gerr) || gerr.Code != tc.status {
					t.Fatalf("%s = %v: the SDK's error is not in the chain", tc.name, err)
				}
			}
		})
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
