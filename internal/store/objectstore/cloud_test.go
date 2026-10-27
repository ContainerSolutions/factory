package objectstore

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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

// TestParseURLClouds: ParseURL opens gs:// and s3:// URLs, with no network: the GCS client
// on an emulator host needs no credentials, and the S3 config is read from the environment.
// S3 needs a region, and FACTORY_S3_ENDPOINT must be a bare http(s) URL.
func TestParseURLClouds(t *testing.T) {
	t.Setenv("STORAGE_EMULATOR_HOST", "127.0.0.1:1")
	b, err := ParseURL(t.Context(), "gs://bucket/factory/state")
	must(t, err)
	if g, ok := b.(*GCSBucket); !ok || g.String() != "gs://bucket/factory/state/" {
		t.Fatalf("ParseURL(gs://) = %#v", b)
	}
	must(t, b.(*GCSBucket).Close())
	if _, err := ParseURL(t.Context(), "gs://b/p?project=x"); err == nil {
		t.Fatal("gs:// with a query was accepted")
	}

	s3TestEnv(t, "")
	b, err = ParseURL(t.Context(), "s3://bucket/factory")
	must(t, err)
	if s, ok := b.(*S3Bucket); !ok || s.String() != "s3://bucket/factory/" {
		t.Fatalf("ParseURL(s3://) = %#v", b)
	}
	for _, ep := range []string{"http://127.0.0.1:9000", "https://minio.test/"} {
		t.Setenv(S3EndpointEnv, ep)
		if _, err := ParseURL(t.Context(), "s3://bucket"); err != nil {
			t.Fatalf("%s=%s: %v", S3EndpointEnv, ep, err)
		}
	}
	for _, ep := range []string{"minio:9000", "ftp://h", "http://h/path", "http://u:hunter2@h", "http://h?x=1", "http://h:x:y"} {
		t.Setenv(S3EndpointEnv, ep)
		_, err := ParseURL(t.Context(), "s3://bucket")
		if err == nil || !strings.Contains(err.Error(), S3EndpointEnv) {
			t.Fatalf("%s=%s = %v, want refused", S3EndpointEnv, ep, err)
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

// TestGCSErrorMapping: a 404 on a read or a delete is ErrNotExist, a 412 on a create is
// ErrExists, and other statuses map to neither. 4xx are not retried, so each case is one
// request against a canned server.
func TestGCSErrorMapping(t *testing.T) {
	gcsError := func(code int) string {
		return fmt.Sprintf(`{"error":{"code":%d,"message":"canned","errors":[{"reason":"canned"}]}}`, code)
	}
	for _, tc := range []struct {
		name   string
		op     func(b Bucket) error
		status int
		want   string
	}{
		{"get 404", func(b Bucket) error { _, err := b.Get(t.Context(), "log/1"); return err }, 404, "ErrNotExist"},
		{"get 403", func(b Bucket) error { _, err := b.Get(t.Context(), "log/1"); return err }, 403, "other"},
		{"create 412", func(b Bucket) error { return b.Create(t.Context(), "log/1", []byte("x")) }, 412, "ErrExists"},
		{"create 403", func(b Bucket) error { return b.Create(t.Context(), "log/1", []byte("x")) }, 403, "other"},
		{"create 404 (no bucket)", func(b Bucket) error { return b.Create(t.Context(), "log/1", []byte("x")) }, 404, "other"},
		{"delete 404", func(b Bucket) error { return b.Delete(t.Context(), "log/1") }, 404, "ErrNotExist"},
		{"delete 403", func(b Bucket) error { return b.Delete(t.Context(), "log/1") }, 403, "other"},
		{"list 404 (no bucket)", func(b Bucket) error {
			return b.List(t.Context(), "log/", "", func(ObjectInfo) error { return nil })
		}, 404, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newCannedServer(t, func(string, int) (int, string) { return tc.status, gcsError(tc.status) })
			t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
			b, err := newGCSBucket(t.Context(), cloudLocation{bucket: "b", prefix: "p/"}, nil)
			must(t, err)
			defer func() { _ = b.Close() }()
			if got := sentinel(tc.op(b)); got != tc.want {
				t.Fatalf("%s = %s, want %s", tc.name, got, tc.want)
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
