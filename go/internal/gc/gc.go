// Package gc is the daily derived-data collector
// (docs/operations/recovery.md#retention-deletion-and-garbage-collection).
// It prunes change history behind the history floor and purges search
// index versions that may still hold destroyed Emails. It has no access to
// the original-mail bucket; uploads expire by S3 lifecycle and table TTL.
package gc

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zero11-0203/s3mail/go/internal/model"
	"github.com/zero11-0203/s3mail/go/internal/store"
)

// DestroyLookback is how far back a destroyed Email makes older index
// versions purgeable; it covers the seven-day deletion deadline with daily
// runs and a lagging index.
const DestroyLookback = 8 * 24 * time.Hour

// Store is the account data the collector reads and prunes.
type Store interface {
	State(ctx context.Context) (model.State, error)
	SearchIndex(ctx context.Context) (model.SearchIndex, bool, error)
	PruneHistory(ctx context.Context, w store.HistoryWriter, keep int64) (store.Pruned, error)
	EmailDestroyedSince(ctx context.Context, since time.Time, upto int64) (bool, error)
}

// IndexS3 is the subset of the S3 client used on the index bucket.
type IndexS3 interface {
	ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, opts ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// Collector runs one collection pass.
type Collector struct {
	Store   Store
	History store.HistoryWriter
	S3      IndexS3
	Bucket  string // the index bucket
	Log     *slog.Logger
	Now     func() time.Time
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Run prunes history, then purges index versions older than the
// registered generation when an Email it no longer holds was destroyed
// within DestroyLookback.
func (c *Collector) Run(ctx context.Context) error {
	st, err := c.Store.State(ctx)
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	reg, ok, err := c.Store.SearchIndex(ctx)
	if err != nil {
		return fmt.Errorf("read search index: %w", err)
	}
	// The indexer and search reader read changes after the registered
	// sequence; an index of another epoch is rebuilt from live Emails.
	keep := int64(0)
	if ok {
		keep = st.Seq
		if reg.Epoch == st.Epoch {
			keep = reg.Seq
		}
	}
	p, err := c.Store.PruneHistory(ctx, c.History, keep)
	if err != nil {
		return fmt.Errorf("prune history: %w", err)
	}
	purged := 0
	if ok && reg.Epoch == st.Epoch {
		destroyed, err := c.Store.EmailDestroyedSince(ctx, c.now().Add(-DestroyLookback), reg.Seq)
		if err != nil {
			return fmt.Errorf("read destroyed emails: %w", err)
		}
		if destroyed {
			if purged, err = c.purgeIndex(ctx, reg); err != nil {
				return fmt.Errorf("purge index versions: %w", err)
			}
		}
	}
	c.Log.Info("gc", "changesDeleted", p.Deleted, "historyFloor", p.Floor, "indexVersionsDeleted", purged)
	return nil
}

// purgeIndex deletes every version of the registered index object older
// than the registered one, which no longer holds the destroyed Emails.
// The registered and newer versions stay.
func (c *Collector) purgeIndex(ctx context.Context, reg model.SearchIndex) (int, error) {
	type version struct {
		id string
		at time.Time
	}
	var all []version
	var cutoff time.Time
	in := &s3.ListObjectVersionsInput{Bucket: &c.Bucket, Prefix: &reg.Key}
	for {
		out, err := c.S3.ListObjectVersions(ctx, in)
		if err != nil {
			return 0, err
		}
		for _, v := range out.Versions {
			if aws.ToString(v.Key) != reg.Key {
				continue
			}
			id, at := aws.ToString(v.VersionId), aws.ToTime(v.LastModified)
			if id == reg.VersionID {
				cutoff = at
			}
			all = append(all, version{id, at})
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		in.KeyMarker, in.VersionIdMarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
	if cutoff.IsZero() {
		return 0, fmt.Errorf("registered version %s of %s not found", reg.VersionID, reg.Key)
	}
	n := 0
	for _, v := range all {
		if v.id == reg.VersionID || !v.at.Before(cutoff) {
			continue
		}
		if _, err := c.S3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &c.Bucket, Key: &reg.Key, VersionId: aws.String(v.id)}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
