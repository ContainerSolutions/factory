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
// '-', '_' and '.', and a prefix of path segments of letters, digits, '-', '_' and '.', other
// than "." and "..". Nothing else is allowed: no user, port, query or fragment, since no key
// or secret ever comes from the URL (credentials come from the platform's identity).
//
// Its errors never print any part of the URL but its scheme: a secret with '/', '?', '#' or
// '"' in it lands outside the userinfo (s3://KEY:/secret@bucket/p puts it in the path), so
// no masking of one part keeps it out. They say which part is wrong instead.
func parseCloudURL(u *url.URL) (cloudLocation, error) {
	bad := func(why string) (cloudLocation, error) {
		return cloudLocation{}, fmt.Errorf("objectstore: the %s:// state URL is refused: %s (want %s://bucket/prefix)", u.Scheme, why, u.Scheme)
	}
	switch {
	case u.Opaque != "":
		return bad("it is not a bucket URL")
	case u.User != nil || strings.Contains(u.Host, "@") || strings.Contains(u.Path, "@"):
		return bad("it has credentials ('@'); they come from the platform's identity, never the URL")
	case strings.Contains(u.Host, ":"):
		return bad("its host has a port or a ':'")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "":
		return bad("it has a query or a fragment")
	case u.RawPath != "":
		return bad("its prefix has escaped characters")
	}
	bucket := u.Hostname()
	if bucket == "" {
		return bad("it has no bucket")
	}
	for _, c := range bucket {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return bad("its bucket name may only have a-z, 0-9, '-', '_' and '.'")
		}
	}
	prefix := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")
	if prefix == "" {
		return cloudLocation{bucket: bucket}, nil
	}
	for seg := range strings.SplitSeq(prefix, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return bad("its prefix has an empty, '.' or '..' segment")
		}
		for _, c := range seg {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
				return bad("its prefix may only have a-z, A-Z, 0-9, '-', '_', '.' and '/'")
			}
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
