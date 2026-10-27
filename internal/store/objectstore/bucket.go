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
)

// Bucket is the little the object store needs from object storage. Names are paths with '/'
// separators (log/…, snapshots/…, transcripts/…), relative to the store's prefix.
type Bucket interface {
	// Get returns the object's bytes. ErrNotExist when there is none.
	Get(ctx context.Context, name string) ([]byte, error)
	// Create creates the object only if the name is free (GCS ifGenerationMatch=0, S3
	// If-None-Match: *, O_EXCL). ErrExists when it is taken. An error other than ErrExists
	// leaves the outcome unknown: the object may have been created.
	Create(ctx context.Context, name string, data []byte) error
	// Put writes the object unconditionally (transcript blobs, which a re-put replaces).
	Put(ctx context.Context, name string, data []byte) error
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

// Opener opens the bucket a parsed state URL names.
type Opener func(u *url.URL) (Bucket, error)

var (
	schemesMu sync.Mutex
	schemes   = map[string]Opener{"file": openFileURL}
)

// RegisterScheme makes ParseURL open URLs of scheme with open (gs and s3 register theirs).
// Registering a scheme again replaces it.
func RegisterScheme(scheme string, open Opener) {
	schemesMu.Lock()
	defer schemesMu.Unlock()
	schemes[strings.ToLower(scheme)] = open
}

// ParseURL opens the bucket a state URL names: file:///abs/dir here (FileBucket), and the
// schemes registered with RegisterScheme. An unknown scheme is an error naming the known ones.
func ParseURL(rawURL string) (Bucket, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("objectstore: state URL %q: %w", rawURL, err)
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
		return nil, fmt.Errorf("objectstore: state URL %q: unknown scheme %q (known: %s)", rawURL, u.Scheme, strings.Join(known, ", "))
	}
	return open(u)
}

// openFileURL opens file:///abs/dir (or file://localhost/abs/dir).
func openFileURL(u *url.URL) (Bucket, error) {
	if u.Opaque != "" || (u.Host != "" && u.Host != "localhost") || u.Path == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("objectstore: state URL %q: want file:///absolute/dir", u.Redacted())
	}
	return NewFileBucket(u.Path)
}

// Object names. Versions are zero-padded to 20 digits, so a listing comes back in version
// order.
const (
	logPrefix        = "log/"
	snapshotPrefix   = "snapshots/"
	transcriptPrefix = "transcripts/"
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
