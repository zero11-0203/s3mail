package store

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// S3Writer is the subset of the client the archive and upload stores use.
type S3Writer interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Archive writes durable MIME for imported and composed Emails to the
// original bucket (docs/operations/recovery.md#original-mail-retention).
// Keys are never reused, so a write never replaces an object.
type Archive struct {
	S3     S3Writer
	Bucket string
}

// Put stores raw under <kind>/<id> and returns its exact version.
func (a *Archive) Put(ctx context.Context, kind, id string, raw []byte) (model.BlobRef, error) {
	if kind != "composed" && kind != "imported" {
		return model.BlobRef{}, fmt.Errorf("unknown archive kind %q", kind)
	}
	key := kind + "/" + id
	out, err := a.S3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &a.Bucket,
		Key:           &key,
		Body:          bytes.NewReader(raw),
		ContentLength: aws.Int64(int64(len(raw))),
		ContentType:   aws.String("message/rfc822"),
		IfNoneMatch:   aws.String("*"),
	})
	if err != nil {
		return model.BlobRef{}, err
	}
	if aws.ToString(out.VersionId) == "" {
		return model.BlobRef{}, fmt.Errorf("archive bucket %s returned no version id", a.Bucket)
	}
	return model.BlobRef{Bucket: a.Bucket, Key: key, VersionID: aws.ToString(out.VersionId)}, nil
}

// Uploads stores uploaded blobs in the disposable bucket, whose lifecycle
// expires them (docs/operations/recovery.md#retention-deletion-and-garbage-collection).
type Uploads struct {
	S3     S3Writer
	Bucket string
}

// MaxUploadBytes bounds an upload read; uploads are capped well below.
const MaxUploadBytes = 8 << 20

// Put stores data under uploads/<id>.
func (u *Uploads) Put(ctx context.Context, id, contentType string, data []byte) (model.BlobRef, error) {
	key := "uploads/" + id
	_, err := u.S3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:               &u.Bucket,
		Key:                  &key,
		Body:                 bytes.NewReader(data),
		ContentLength:        aws.Int64(int64(len(data))),
		ContentType:          aws.String(contentType),
		IfNoneMatch:          aws.String("*"),
		ServerSideEncryption: types.ServerSideEncryptionAes256,
	})
	if err != nil {
		return model.BlobRef{}, err
	}
	return model.BlobRef{Bucket: u.Bucket, Key: key}, nil
}

// Get reads an uploaded blob in full.
func (u *Uploads) Get(ctx context.Context, ref model.BlobRef) ([]byte, error) {
	out, err := u.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: &ref.Bucket, Key: &ref.Key})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(out.Body, MaxUploadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxUploadBytes {
		return nil, fmt.Errorf("upload %s exceeds %d bytes", ref.Key, MaxUploadBytes)
	}
	return b, nil
}
