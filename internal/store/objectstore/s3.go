package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3EndpointEnv names the environment variable that points S3 buckets at an S3-compatible
// endpoint (MinIO, for the emulator tests) instead of AWS. When it is set, requests use
// path-style addressing (http://endpoint/bucket/key), which such servers expect. Credentials
// and the region still come from the default chain (AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY, AWS_REGION).
const S3EndpointEnv = "FACTORY_S3_ENDPOINT"

// s3ConflictRetries is how many times Create re-sends a PUT S3 answered 409
// ConditionalRequestConflict (another conditional write to the key is in flight), and
// s3ConflictBackoff the first wait between them, doubled each time.
const (
	s3ConflictRetries = 3
	s3ConflictBackoff = 100 * time.Millisecond
)

// S3Bucket is a Bucket on Amazon S3 (s3://bucket/prefix), or on an S3-compatible server when
// FACTORY_S3_ENDPOINT is set. Credentials come from the SDK's default chain (Lambda's role,
// or the environment); none come from a flag or the config.
//
//   - Create is PutObject with If-None-Match: *; a 412 is ErrExists. The SDK retries a PUT
//     that failed with a 5xx or a timeout, so a retry whose first attempt landed answers
//     ErrExists, which the store's nonce check resolves. A 409 ConditionalRequestConflict
//     (a conditional write to the key in flight) is re-sent up to 3 times after a short wait;
//     when it persists, the outcome is unknown, as for a timeout.
//   - Put is an unconditional PutObject.
//   - Get answers ErrNotExist on NoSuchKey (not on NoSuchBucket).
//   - List is ListObjectsV2 with StartAfter, in name order.
//   - Delete is HeadObject, then DeleteObject: S3's delete succeeds whether or not the key
//     exists, and the Bucket contract answers ErrNotExist for a missing object.
//
// Each request's deadline is its context's: the store gives each one DefaultRequestTimeout.
type S3Bucket struct {
	client *s3.Client
	loc    cloudLocation
}

var _ Bucket = (*S3Bucket)(nil)

// NewS3Bucket opens s3://bucket/prefix with the default credential chain, and
// FACTORY_S3_ENDPOINT when it is set. prefix is "" or a path without leading or trailing '/'.
func NewS3Bucket(ctx context.Context, bucket, prefix string) (*S3Bucket, error) {
	loc, err := parseCloudURL(&url.URL{Scheme: "s3", Host: bucket, Path: "/" + prefix})
	if err != nil {
		return nil, err
	}
	return newS3Bucket(ctx, loc, nil)
}

func openS3URL(ctx context.Context, u *url.URL) (Bucket, error) {
	loc, err := parseCloudURL(u)
	if err != nil {
		return nil, err
	}
	return newS3Bucket(ctx, loc, nil)
}

// newS3Bucket opens loc; hc, when set, is the client's HTTP client (tests wrap the transport
// to fail a request after it landed).
func newS3Bucket(ctx context.Context, loc cloudLocation, hc *http.Client) (*S3Bucket, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("objectstore: s3://%s: %w", loc.bucket, err)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("objectstore: s3://%s: no region: set AWS_REGION (Lambda sets it)", loc.bucket)
	}
	endpoint, err := s3Endpoint()
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		}
		if hc != nil {
			o.HTTPClient = hc
		}
	})
	return &S3Bucket{client: client, loc: loc}, nil
}

// s3Endpoint is FACTORY_S3_ENDPOINT, checked: an http or https URL with a host and nothing
// after it.
func s3Endpoint() (string, error) {
	raw := os.Getenv(S3EndpointEnv)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("objectstore: %s=%q: want http(s)://host[:port]", S3EndpointEnv, raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// String is the bucket's URL.
func (b *S3Bucket) String() string { return "s3://" + b.loc.bucket + "/" + b.loc.prefix }

// Get implements Bucket.
func (b *S3Bucket) Get(ctx context.Context, name string) ([]byte, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &b.loc.bucket, Key: aws.String(b.loc.prefix + name)})
	if err != nil {
		return nil, b.err(ctx, "get", name, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, b.err(ctx, "get", name, err)
	}
	return data, nil
}

// Create implements Bucket: PutObject with If-None-Match: *.
func (b *S3Bucket) Create(ctx context.Context, name string, data []byte) error {
	wait := s3ConflictBackoff
	for attempt := 0; ; attempt++ {
		err := b.put(ctx, name, data, true)
		if err == nil || attempt == s3ConflictRetries || !isS3Conflict(err) {
			if err != nil {
				return b.err(ctx, "create", name, err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return b.err(ctx, "create", name, err)
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// Put implements Bucket: an unconditional PutObject.
func (b *S3Bucket) Put(ctx context.Context, name string, data []byte) error {
	if err := b.put(ctx, name, data, false); err != nil {
		return b.err(ctx, "put", name, err)
	}
	return nil
}

func (b *S3Bucket) put(ctx context.Context, name string, data []byte, ifAbsent bool) error {
	if name == "" {
		return errors.New("empty object name")
	}
	in := &s3.PutObjectInput{
		Bucket:        &b.loc.bucket,
		Key:           aws.String(b.loc.prefix + name),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String("application/octet-stream"),
	}
	if ifAbsent {
		in.IfNoneMatch = aws.String("*")
	}
	_, err := b.client.PutObject(ctx, in)
	return err
}

// List implements Bucket.
func (b *S3Bucket) List(ctx context.Context, prefix, startAfter string, fn func(ObjectInfo) error) error {
	in := &s3.ListObjectsV2Input{Bucket: &b.loc.bucket, Prefix: aws.String(b.loc.prefix + prefix)}
	if startAfter != "" {
		in.StartAfter = aws.String(b.loc.prefix + startAfter)
	}
	pages := s3.NewListObjectsV2Paginator(b.client, in)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return b.err(ctx, "list", prefix, err)
		}
		for _, o := range page.Contents {
			key := aws.ToString(o.Key)
			name, ok := strings.CutPrefix(key, b.loc.prefix)
			if !ok {
				return fmt.Errorf("s3://%s: list %s: key %q is outside the prefix", b.loc.bucket, prefix, key)
			}
			info := ObjectInfo{Name: name, Size: aws.ToInt64(o.Size), Updated: aws.ToTime(o.LastModified)}
			if err := fn(info); err != nil {
				return err
			}
		}
	}
	return nil
}

// Delete implements Bucket: HeadObject for ErrNotExist, then DeleteObject.
func (b *S3Bucket) Delete(ctx context.Context, name string) error {
	key := aws.String(b.loc.prefix + name)
	if _, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b.loc.bucket, Key: key}); err != nil {
		return b.err(ctx, "delete", name, err)
	}
	if _, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b.loc.bucket, Key: key}); err != nil {
		return b.err(ctx, "delete", name, err)
	}
	return nil
}

// err maps an S3 error to the Bucket's (s3Sentinel). Anything else is returned as it is,
// matching ctx's error when the context ended.
func (b *S3Bucket) err(ctx context.Context, op, name string, err error) error {
	if mapped := s3Sentinel(op, err); mapped != nil {
		err = mapped
	} else {
		err = withCtxErr(ctx, err)
	}
	return fmt.Errorf("s3://%s/%s%s: %s: %w", b.loc.bucket, b.loc.prefix, name, op, err)
}

// s3Sentinel is ErrNotExist or ErrExists for the errors that mean them, nil otherwise:
//   - create: 412 (PreconditionFailed) is ErrExists;
//   - get: NoSuchKey is ErrNotExist (NoSuchBucket, also a 404, is not);
//   - delete: HeadObject's 404 is ErrNotExist (a HEAD has no body, so a missing bucket looks
//     the same; the store ignores ErrNotExist on a delete either way).
func s3Sentinel(op string, err error) error {
	status := 0
	if re, ok := errors.AsType[*awshttp.ResponseError](err); ok {
		status = re.HTTPStatusCode()
	}
	code := ""
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		code = ae.ErrorCode()
	}
	switch op {
	case "create":
		if status == http.StatusPreconditionFailed || code == "PreconditionFailed" {
			return ErrExists
		}
	case "get":
		if code == "NoSuchKey" {
			return ErrNotExist
		}
	case "delete":
		if status == http.StatusNotFound && (code == "NotFound" || code == "NoSuchKey" || code == "") {
			return ErrNotExist
		}
	}
	return nil
}

// isS3Conflict reports a 409 ConditionalRequestConflict: a conditional write to the key is
// in flight, and S3 asks for the request to be sent again.
func isS3Conflict(err error) bool {
	ae, ok := errors.AsType[smithy.APIError](err)
	return ok && ae.ErrorCode() == "ConditionalRequestConflict"
}
