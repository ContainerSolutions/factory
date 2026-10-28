package objectstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/store/journal"
)

// Bucket is the little the object store needs from object storage. Names are paths with '/'
// separators (log/…, snapshots/…, transcripts/…), relative to the store's prefix. Every object
// is created once and never overwritten (transcript blobs too: each put has its own name).
type Bucket interface {
	// Get returns the object's bytes. ErrNotExist when there is none.
	Get(ctx context.Context, name string) ([]byte, error)
	// Create creates the object only if the name is free (GCS ifGenerationMatch=0, S3
	// If-None-Match: *, O_EXCL). ErrExists when it is taken. An error other than ErrExists
	// leaves the outcome unknown: the object may have been created.
	Create(ctx context.Context, name string, data []byte) error
	// List calls fn with the objects whose names start with prefix and sort after startAfter
	// (all of them when it is ""), in name order. An error from fn stops the listing and is
	// returned.
	List(ctx context.Context, prefix, startAfter string, fn func(ObjectInfo) error) error
	// Delete removes the object. ErrNotExist when there is none.
	Delete(ctx context.Context, name string) error
}

// ObjectInfo is one listed object.
type ObjectInfo struct {
	Name string
	Size int64
	// Updated is when the object was last written, on the bucket's clock.
	Updated time.Time
}

// Bucket errors (test with errors.Is).
var (
	// ErrNotExist: no object with that name.
	ErrNotExist = errors.New("object does not exist")
	// ErrExists: Create on a name that is taken.
	ErrExists = errors.New("object already exists")
)

// Opener opens the bucket a parsed state URL names. ctx bounds the opening (finding
// credentials, reading the SDK's config), not the bucket's later requests.
type Opener func(ctx context.Context, u *url.URL) (Bucket, error)

var (
	schemesMu sync.Mutex
	schemes   = map[string]Opener{"file": openFileURL, "gs": openGCSURL, "s3": openS3URL}
)

// RegisterScheme makes ParseURL open URLs of scheme with open (tests register their own).
// Registering a scheme again replaces it.
func RegisterScheme(scheme string, open Opener) {
	schemesMu.Lock()
	defer schemesMu.Unlock()
	schemes[strings.ToLower(scheme)] = open
}

// ParseURL opens the bucket a state URL names: file:///abs/dir (FileBucket),
// gs://bucket/prefix (GCSBucket), s3://bucket/prefix (S3Bucket), and the schemes registered
// with RegisterScheme. An unknown scheme is an error naming the known ones.
//
// A state URL may carry a credential anywhere (s3://key:secret@…, and a secret holding '/',
// '?', '#' or '"' doesn't even parse as userinfo), so no error about it prints any of it but
// its scheme: each says which part is wrong instead.
func ParseURL(ctx context.Context, rawURL string) (Bucket, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("objectstore: the state URL doesn't parse")
	}
	schemesMu.Lock()
	open := schemes[strings.ToLower(u.Scheme)]
	known := make([]string, 0, len(schemes))
	for s := range schemes {
		known = append(known, s+"://")
	}
	schemesMu.Unlock()
	if open == nil {
		slices.Sort(known)
		return nil, fmt.Errorf("objectstore: state URL: unknown scheme %q (known: %s)", u.Scheme, strings.Join(known, ", "))
	}
	return open(ctx, u)
}

// openFileURL opens file:///abs/dir (or file://localhost/abs/dir). Its errors name the part
// that is wrong, never the URL.
func openFileURL(_ context.Context, u *url.URL) (Bucket, error) {
	var wrong string
	switch {
	case u.User != nil:
		wrong = "the URL has userinfo"
	case u.Host != "" && u.Host != "localhost":
		wrong = "the URL has a host"
	case u.Opaque != "" || !strings.HasPrefix(u.Path, "/"):
		wrong = "the URL has a relative path"
	case u.RawQuery != "" || u.ForceQuery:
		wrong = "the URL has a query"
	case u.Fragment != "":
		wrong = "the URL has a fragment"
	}
	if wrong != "" {
		return nil, fmt.Errorf("objectstore: state URL: want file:///absolute/dir: %s", wrong)
	}
	return NewFileBucket(u.Path)
}

// Object names. Versions are zero-padded to 20 digits, so a listing comes back in version
// order.
const (
	logPrefix        = journal.ObjectStoreLog + "/"
	snapshotPrefix   = journal.ObjectStoreSnapshots + "/"
	transcriptPrefix = journal.TranscriptDir + "/"
	logSuffix        = ".json"
	snapshotSuffix   = ".json.gz"
)

func logName(v uint64) string { return fmt.Sprintf("%s%020d%s", logPrefix, v, logSuffix) }
func snapshotName(v uint64) string {
	return fmt.Sprintf("%s%020d%s", snapshotPrefix, v, snapshotSuffix)
}

// versionOf parses the version out of a log or snapshot name; ok is false for a name the
// store doesn't write.
func versionOf(name, prefix, suffix string) (uint64, bool) {
	digits, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return 0, false
	}
	if digits, ok = strings.CutSuffix(digits, suffix); !ok || len(digits) != 20 {
		return 0, false
	}
	var v uint64
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := uint64(c - '0')
		if v > (^uint64(0)-d)/10 {
			return 0, false
		}
		v = v*10 + d
	}
	return v, v > 0
}
