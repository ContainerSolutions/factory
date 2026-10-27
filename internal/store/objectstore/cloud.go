package objectstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// cloudLocation is where a gs:// or s3:// URL puts the store: a bucket, and a prefix that is
// "" or ends in "/".
type cloudLocation struct {
	bucket, prefix string
}

// parseCloudURL reads scheme://bucket[/prefix]: a bucket name of lower-case letters, digits,
// '-', '_' and '.', and a prefix of non-empty path segments other than "." and "..". Nothing
// else is allowed: no user, port, query or fragment, since no key or secret ever comes from
// the URL (credentials come from the platform's identity).
func parseCloudURL(u *url.URL) (cloudLocation, error) {
	bad := func(why string) (cloudLocation, error) {
		return cloudLocation{}, fmt.Errorf("objectstore: state URL %q: %s (want %s://bucket/prefix)", u.Redacted(), why, u.Scheme)
	}
	switch {
	case u.Opaque != "":
		return bad("not a bucket URL")
	case u.User != nil:
		return bad("credentials in the URL are refused: they come from the platform's identity")
	case u.Port() != "":
		return bad("a port is not a bucket")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return bad("no query or fragment")
	case u.RawPath != "":
		return bad("the prefix has escaped characters")
	}
	bucket := u.Hostname()
	if bucket == "" {
		return bad("no bucket")
	}
	for _, c := range bucket {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return bad(fmt.Sprintf("bucket name %q: only a-z, 0-9, '-', '_' and '.'", bucket))
		}
	}
	prefix := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")
	if prefix == "" {
		return cloudLocation{bucket: bucket}, nil
	}
	for seg := range strings.SplitSeq(prefix, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return bad(fmt.Sprintf("prefix %q has an empty, '.' or '..' segment", prefix))
		}
	}
	return cloudLocation{bucket: bucket, prefix: prefix + "/"}, nil
}

// withCtxErr makes sure an error from a request whose context ended matches ctx's error
// (context.Canceled, context.DeadlineExceeded) with errors.Is, whatever the SDK wrapped.
func withCtxErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && err != nil && !errors.Is(err, cerr) {
		return fmt.Errorf("%w: %w", cerr, err)
	}
	return err
}
