package gc

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zero11-0203/s3mail/go/internal/model"
	"github.com/zero11-0203/s3mail/go/internal/store"
)

var t0 = time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)

type fakeStore struct {
	st        model.State
	reg       model.SearchIndex
	hasIndex  bool
	destroyed bool
	keep      int64
	upto      int64
}

func (f *fakeStore) State(context.Context) (model.State, error) { return f.st, nil }

func (f *fakeStore) SearchIndex(context.Context) (model.SearchIndex, bool, error) {
	return f.reg, f.hasIndex, nil
}

func (f *fakeStore) PruneHistory(_ context.Context, _ store.HistoryWriter, keep int64) (store.Pruned, error) {
	f.keep = keep
	return store.Pruned{}, nil
}

func (f *fakeStore) EmailDestroyedSince(_ context.Context, since time.Time, upto int64) (bool, error) {
	f.upto = upto
	return f.destroyed && since.Equal(t0.Add(-DestroyLookback)), nil
}

type fakeS3 struct {
	versions []s3types.ObjectVersion
	deleted  []string
}

func (f *fakeS3) ListObjectVersions(_ context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	// One version per page exercises the markers.
	i := 0
	if in.VersionIdMarker != nil {
		i = slices.IndexFunc(f.versions, func(v s3types.ObjectVersion) bool { return *v.VersionId == *in.VersionIdMarker }) + 1
	}
	out := &s3.ListObjectVersionsOutput{Versions: f.versions[i : i+1], IsTruncated: aws.Bool(i+1 < len(f.versions))}
	out.NextKeyMarker, out.NextVersionIdMarker = f.versions[i].Key, f.versions[i].VersionId
	return out, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if aws.ToString(in.VersionId) == "" {
		panic("a delete without a version id would add a delete marker")
	}
	f.deleted = append(f.deleted, *in.Key+"@"+*in.VersionId)
	return &s3.DeleteObjectOutput{}, nil
}

func version(key, id string, age time.Duration) s3types.ObjectVersion {
	return s3types.ObjectVersion{Key: aws.String(key), VersionId: aws.String(id), LastModified: aws.Time(t0.Add(-age))}
}

func collector(st *fakeStore, objects *fakeS3) *Collector {
	return &Collector{Store: st, S3: objects, Bucket: "s3mail-index-test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return t0 }}
}

func TestPurgeAfterDestroy(t *testing.T) {
	key := "index/primary.sqlite"
	st := &fakeStore{
		st:  model.State{Seq: 50, Epoch: 1},
		reg: model.SearchIndex{Seq: 48, Epoch: 1, Key: key, VersionID: "v3"}, hasIndex: true, destroyed: true,
	}
	objects := &fakeS3{versions: []s3types.ObjectVersion{
		version(key, "v4", time.Minute), // published, not yet registered
		version(key, "v3", time.Hour),
		version(key+".other", "x1", 3*time.Hour),
		version(key, "v2", 2*time.Hour),
		version(key, "v1", 3*time.Hour),
	}}
	if err := collector(st, objects).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st.keep != 48 || st.upto != 48 {
		t.Errorf("keep %d upto %d", st.keep, st.upto)
	}
	if !slices.Equal(objects.deleted, []string{key + "@v2", key + "@v1"}) {
		t.Errorf("deleted %v", objects.deleted)
	}
}

func TestNoPurgeWithoutDestroy(t *testing.T) {
	st := &fakeStore{st: model.State{Seq: 50, Epoch: 1}, reg: model.SearchIndex{Seq: 48, Epoch: 1, Key: "k", VersionID: "v2"}, hasIndex: true}
	objects := &fakeS3{versions: []s3types.ObjectVersion{version("k", "v2", 0), version("k", "v1", time.Hour)}}
	if err := collector(st, objects).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(objects.deleted) != 0 {
		t.Errorf("deleted %v", objects.deleted)
	}
}

func TestHistoryKeep(t *testing.T) {
	for _, c := range []struct {
		name     string
		hasIndex bool
		epoch    int64
		want     int64
	}{
		{"no index: history is kept", false, 1, 0},
		{"index of another epoch is rebuilt", true, 1, 50},
	} {
		st := &fakeStore{st: model.State{Seq: 50, Epoch: 2}, reg: model.SearchIndex{Seq: 10, Epoch: c.epoch}, hasIndex: c.hasIndex, destroyed: true}
		if err := collector(st, &fakeS3{}).Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		if st.keep != c.want || st.upto != 0 {
			t.Errorf("%s: keep %d upto %d", c.name, st.keep, st.upto)
		}
	}
}

func TestPurgeNeedsRegisteredVersion(t *testing.T) {
	st := &fakeStore{st: model.State{Seq: 5, Epoch: 1}, reg: model.SearchIndex{Seq: 5, Epoch: 1, Key: "k", VersionID: "gone"}, hasIndex: true, destroyed: true}
	objects := &fakeS3{versions: []s3types.ObjectVersion{version("k", "v1", time.Hour)}}
	if err := collector(st, objects).Run(t.Context()); err == nil || len(objects.deleted) != 0 {
		t.Errorf("err %v deleted %v", err, objects.deleted)
	}
}
