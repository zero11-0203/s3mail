package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// S3 is the subset of the client the original reader uses: reads only.
type S3 interface {
	ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, opts ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Originals reads delivered objects from the permanent original bucket.
type Originals struct {
	S3  S3
	Log *slog.Logger
}

// ResolveVersion returns the oldest version of key. SES writes each key
// once, so that is its delivery; a second version is logged as an anomaly
// and never chosen.
func (o *Originals) ResolveVersion(ctx context.Context, bucket, key string) (string, error) {
	out, err := o.S3.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket:  &bucket,
		Prefix:  &key,
		MaxKeys: aws.Int32(10),
	})
	if err != nil {
		return "", err
	}
	var oldest string
	var oldestAt time.Time
	var count int
	for _, v := range out.Versions {
		if aws.ToString(v.Key) != key || v.LastModified == nil {
			continue
		}
		count++
		if oldest == "" || v.LastModified.Before(oldestAt) {
			oldest, oldestAt = aws.ToString(v.VersionId), *v.LastModified
		}
	}
	switch {
	case count == 0:
		return "", fmt.Errorf("no version of %s", key)
	case count > 1:
		o.Log.Warn("original has more than one version", "key", key, "versions", count)
	}
	return oldest, nil
}

// ReadPrefix returns up to n bytes from the start of one version and the
// object's full size.
func (o *Originals) ReadPrefix(ctx context.Context, bucket, key, version string, n int64) ([]byte, int64, error) {
	out, err := o.S3.GetObject(ctx, &s3.GetObjectInput{
		Bucket:    &bucket,
		Key:       &key,
		VersionId: &version,
		Range:     aws.String(fmt.Sprintf("bytes=0-%d", n-1)),
	})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(out.Body, n))
	if err != nil {
		return nil, 0, err
	}
	size, err := totalSize(aws.ToString(out.ContentRange), int64(len(b)))
	return b, size, err
}

// totalSize reads the size from "bytes 0-99/1234"; without a range the
// body was the whole object.
func totalSize(contentRange string, read int64) (int64, error) {
	if contentRange == "" {
		return read, nil
	}
	_, total, ok := strings.Cut(contentRange, "/")
	if !ok || total == "*" {
		return 0, errors.New("unexpected Content-Range " + strconv.Quote(contentRange))
	}
	return strconv.ParseInt(total, 10, 64)
}

// MaxOriginalBytes bounds a full read: the SES S3 action stores at most
// 40 MB per message.
const MaxOriginalBytes = 41 << 20

// Fetch reads one exact original version in full.
func (o *Originals) Fetch(ctx context.Context, ref model.BlobRef) ([]byte, error) {
	if ref.VersionID == "" {
		return nil, fmt.Errorf("no version id for %s", ref.Key)
	}
	out, err := o.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: &ref.Bucket, Key: &ref.Key, VersionId: &ref.VersionID})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(out.Body, MaxOriginalBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxOriginalBytes {
		return nil, fmt.Errorf("original %s exceeds %d bytes", ref.Key, MaxOriginalBytes)
	}
	return b, nil
}
