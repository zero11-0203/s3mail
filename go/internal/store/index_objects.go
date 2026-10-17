package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/zero11-0203/s3mail/go/internal/search"
)

// IndexS3 is the subset of the client the index objects use.
type IndexS3 interface {
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// IndexObjects stores search snapshots in the versioned index bucket.
type IndexObjects struct {
	S3     IndexS3
	Bucket string
}

// MaxIndexBytes bounds a snapshot download; the alert threshold is 1 GiB
// (docs/specs/search.md#why-sqlite-on-s3).
const MaxIndexBytes = 2 << 30

// Head implements search.Objects.
func (o *IndexObjects) Head(ctx context.Context, key string) (search.Object, bool, error) {
	out, err := o.S3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &o.Bucket, Key: &key})
	var ae smithy.APIError
	if errors.As(err, &ae) && (ae.ErrorCode() == "NotFound" || ae.ErrorCode() == "NoSuchKey") {
		return search.Object{}, false, nil
	}
	if err != nil {
		return search.Object{}, false, err
	}
	return search.Object{VersionID: aws.ToString(out.VersionId), ETag: aws.ToString(out.ETag)}, true, nil
}

// Download implements search.Objects.
func (o *IndexObjects) Download(ctx context.Context, key, versionID, path string) error {
	out, err := o.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: &o.Bucket, Key: &key, VersionId: &versionID})
	if err != nil {
		return err
	}
	defer func() { _ = out.Body.Close() }()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(out.Body, MaxIndexBytes+1))
	if err = errors.Join(err, f.Close()); err != nil {
		_ = os.Remove(path)
		return err
	}
	if n > MaxIndexBytes {
		_ = os.Remove(path)
		return fmt.Errorf("index exceeds %d bytes", int64(MaxIndexBytes))
	}
	return nil
}

// Upload implements search.Objects with If-Match on the previous ETag, or
// If-None-Match for the first snapshot.
func (o *IndexObjects) Upload(ctx context.Context, key, path, ifMatch string) (search.Object, error) {
	f, err := os.Open(path)
	if err != nil {
		return search.Object{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return search.Object{}, err
	}
	in := &s3.PutObjectInput{Bucket: &o.Bucket, Key: &key, Body: f, ContentLength: aws.Int64(info.Size()), ContentType: aws.String("application/vnd.sqlite3")}
	if ifMatch == "" {
		in.IfNoneMatch = aws.String("*")
	} else {
		in.IfMatch = aws.String(ifMatch)
	}
	out, err := o.S3.PutObject(ctx, in)
	var ae smithy.APIError
	if errors.As(err, &ae) && (ae.ErrorCode() == "PreconditionFailed" || ae.ErrorCode() == "ConditionalRequestConflict") {
		return search.Object{}, search.ErrConflict
	}
	if err != nil {
		return search.Object{}, err
	}
	if aws.ToString(out.VersionId) == "" {
		return search.Object{}, fmt.Errorf("index bucket %s returned no version id", o.Bucket)
	}
	return search.Object{VersionID: aws.ToString(out.VersionId), ETag: aws.ToString(out.ETag)}, nil
}
