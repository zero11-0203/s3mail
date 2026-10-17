package jmap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/zero11-0203/s3mail/go/internal/mailparse"
	"github.com/zero11-0203/s3mail/go/internal/model"
)

// Archive writes durable MIME for new Emails to the original bucket.
type Archive interface {
	Put(ctx context.Context, kind, id string, raw []byte) (BlobRef, error)
}

// Uploads stores uploaded blobs in the disposable bucket.
type Uploads interface {
	Put(ctx context.Context, id, contentType string, data []byte) (BlobRef, error)
	Get(ctx context.Context, ref BlobRef) ([]byte, error)
}

// uploadLifetime is how long an unattached upload stays usable
// (docs/operations/recovery.md#retention-deletion-and-garbage-collection).
const uploadLifetime = 24 * time.Hour

// blob is resolved blob content.
type blob struct {
	data []byte
	// typ is the upload's or part's media type, "" for a raw message.
	typ string
}

// readBlob resolves a blob id in the caller's account: a live Email's raw
// message or part, or an unexpired upload or a part of an uploaded
// message. Anything else is not found (docs/specs/security.md#shared-endpoint-authorization).
func (s *Server) readBlob(ctx context.Context, blobID string) (blob, bool, error) {
	ref, ok := ParseBlobID(blobID)
	if !ok {
		return blob{}, false, nil
	}
	var b blob
	if ref.upload() {
		u, ok, err := s.Store.GetUpload(ctx, ref.Owner)
		if err != nil || !ok || !s.now().Before(u.ExpiresAt) {
			return blob{}, false, err
		}
		data, err := s.Uploads.Get(ctx, BlobRef{Bucket: u.Bucket, Key: u.Key})
		if err != nil {
			return blob{}, false, err
		}
		b = blob{data: data, typ: u.Type}
	} else {
		found, err := s.Store.GetEmails(ctx, []string{ref.Owner})
		if err != nil {
			return blob{}, false, err
		}
		m, ok := found[ref.Owner]
		if !ok {
			return blob{}, false, nil
		}
		data, err := s.Originals.Fetch(ctx, m.Blob)
		if err != nil {
			return blob{}, false, err
		}
		b = blob{data: data, typ: "message/rfc822"}
	}
	if ref.Part != "" {
		for _, seg := range strings.Split(ref.Part, ".") {
			p := mailparse.Parse(b.data).Find(seg)
			if p == nil {
				return blob{}, false, nil
			}
			b = blob{data: p.Content(), typ: p.Type}
		}
	}
	return b, true, nil
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type uploadResponse struct {
	AccountID string `json:"accountId"`
	BlobID    string `json:"blobId"`
	Type      string `json:"type"`
	Size      int64  `json:"size"`
}

// HandleUpload serves POST /upload/{accountId} (RFC 8620 §6.1): the bytes
// go to the disposable bucket and a BLOB record makes them the account's
// for 24 hours.
func (s *Server) HandleUpload(ctx context.Context, req events.APIGatewayProxyRequest, owners Owners) (*events.APIGatewayProxyStreamingResponse, error) {
	acct, ok := AccountFromRequest(req, owners)
	if !ok || !acct.has(ScopeWrite) {
		return problemResponse(problem("forbidden", 403, "")), nil
	}
	if req.HTTPMethod != http.MethodPost {
		return problemResponse(problem("notFound", 404, "")), nil
	}
	st, err := s.Store.State(ctx)
	if err != nil {
		return problemResponse(problem("serverFail", 500, "")), nil
	}
	s.epoch = st.Epoch
	if req.PathParameters["accountId"] != s.publicID(acct) {
		return problemResponse(problem("notFound", 404, "no such account")), nil
	}
	data := []byte(req.Body)
	if req.IsBase64Encoded {
		if data, err = base64.StdEncoding.DecodeString(req.Body); err != nil {
			return problemResponse(problem("invalidArguments", 400, "undecodable body")), nil
		}
	}
	if len(data) > MaxSizeUpload {
		p := problem("limit", 413, "upload too large")
		p.Limit = "maxSizeUpload"
		return problemResponse(p), nil
	}
	ctype := "application/octet-stream"
	if mt, params, err := mime.ParseMediaType(header(req.Headers, "Content-Type")); err == nil {
		ctype = mime.FormatMediaType(mt, params)
	}
	id := model.NewID("U")
	ref, err := s.Uploads.Put(ctx, id, ctype, data)
	if err != nil {
		s.Log.Error("upload", "outcome", "serverFail", "step", "put", "error", err)
		return problemResponse(problem("serverFail", 500, "")), nil
	}
	now := s.now().UTC()
	u := model.Upload{ID: id, Type: ctype, Size: int64(len(data)), Bucket: ref.Bucket, Key: ref.Key, Created: now, ExpiresAt: now.Add(uploadLifetime)}
	if _, serr := s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
		return &model.Mutation{Uploads: []model.Upload{u}}, nil
	}); serr != nil {
		return problemResponse(problem("serverFail", 500, serr.Description)), nil
	}
	s.Log.Info("upload", "outcome", "ok", "size", len(data))
	return jsonResponse(201, "application/json", uploadResponse{AccountID: s.publicID(acct), BlobID: id, Type: ctype, Size: u.Size}), nil
}

// copyArgs are the shared /copy arguments (RFC 8620 §5.4, §6.3).
type copyArgs struct {
	FromAccountID string          `json:"fromAccountId"`
	AccountID     string          `json:"accountId"`
	BlobIDs       []string        `json:"blobIds"`
	IfFromInState *string         `json:"ifFromInState"`
	IfInState     *string         `json:"ifInState"`
	Create        json.RawMessage `json:"create"`
	OnSuccess     *bool           `json:"onSuccessDestroyOriginal"`
	DestroyFrom   *string         `json:"destroyFromIfInState"`
}

// copyBetweenAccounts answers /copy for the single-owner deployment: the
// two accounts must differ, and no other account exists.
func copyBetweenAccounts(_ context.Context, c *call) (any, *MethodError) {
	var a copyArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	if a.FromAccountID == a.AccountID {
		return nil, methodErr("invalidArguments", "fromAccountId must differ from accountId")
	}
	return nil, methodErr("fromAccountNotFound", "")
}
