package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"
)

// Server holds one deployment's configuration and data access.
type Server struct {
	// BaseURL is the public API root, e.g. https://api.example.com/v1.
	BaseURL   string
	Store     Store
	Originals Originals
	// Archive and Uploads are needed by uploads, import and creation.
	Archive Archive
	Uploads Uploads
	// Searcher answers text conditions and snippets; nil disables them.
	Searcher Searcher
	Log      *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
	// PushPoll overrides PollInterval (tests).
	PushPoll time.Duration

	methods map[string]method
	epoch   int64
}

// NewServer registers every implemented method.
func NewServer(baseURL string, store Store, originals Originals, log *slog.Logger) *Server {
	s := &Server{BaseURL: baseURL, Store: store, Originals: originals, Log: log, epoch: 1}
	s.methods = map[string]method{
		"Core/echo":            {CapCore, ScopeRead, echo},
		"Mailbox/get":          {CapMail, ScopeRead, mailboxGet},
		"Mailbox/changes":      {CapMail, ScopeRead, mailboxChanges},
		"Mailbox/query":        {CapMail, ScopeRead, mailboxQuery},
		"Mailbox/queryChanges": {CapMail, ScopeRead, mailboxQueryChanges},
		"Mailbox/set":          {CapMail, ScopeWrite, mailboxSet},
		"Thread/get":           {CapMail, ScopeRead, threadGet},
		"Thread/changes":       {CapMail, ScopeRead, threadChanges},
		"Email/get":            {CapMail, ScopeRead, emailGet},
		"Email/query":          {CapMail, ScopeRead, emailQuery},
		"Email/changes":        {CapMail, ScopeRead, emailChanges},
		"Email/queryChanges":   {CapMail, ScopeRead, emailQueryChanges},
		"SearchSnippet/get":    {CapMail, ScopeRead, searchSnippetGet},
		"Email/set":            {CapMail, ScopeWrite, emailSet},
		"Email/import":         {CapMail, ScopeWrite, emailImport},
		"Email/parse":          {CapMail, ScopeRead, emailParse},
		"Email/copy":           {CapMail, ScopeWrite, copyBetweenAccounts},
		"Blob/copy":            {CapCore, ScopeWrite, copyBetweenAccounts},
	}
	return s
}

func echo(_ context.Context, c *call) (any, *MethodError) {
	// Core/echo returns its arguments unchanged.
	return c.args, nil
}

func (s *Server) publicID(a Account) string { return PublicAccountID(a.InternalID, s.epoch) }

// consistent runs reads between two strong STATE reads and retries until
// they agree (docs/specs/storage-sync.md#dynamodb-storage-and-consistency-contract).
func (s *Server) consistent(ctx context.Context, read func(State) error) (State, *MethodError) {
	for range 3 {
		before, err := s.Store.State(ctx)
		if err != nil {
			return State{}, s.fail("read state", err)
		}
		s.epoch = before.Epoch
		if err := read(before); err != nil {
			return State{}, s.fail("read", err)
		}
		after, err := s.Store.State(ctx)
		if err != nil {
			return State{}, s.fail("read state", err)
		}
		if after == before {
			return before, nil
		}
	}
	return State{}, methodErr("serverUnavailable", "account changed during the read; retry")
}

func (s *Server) fail(what string, err error) *MethodError {
	s.Log.Error("jmap", "outcome", "serverFail", "step", what, "error", err)
	return methodErr("serverFail", what)
}

// Session is the JMAP Session resource (RFC 8620 §2).
type Session struct {
	Capabilities    map[string]any            `json:"capabilities"`
	Accounts        map[string]SessionAccount `json:"accounts"`
	PrimaryAccounts map[string]string         `json:"primaryAccounts"`
	Username        string                    `json:"username"`
	APIURL          string                    `json:"apiUrl"`
	DownloadURL     string                    `json:"downloadUrl"`
	UploadURL       string                    `json:"uploadUrl"`
	EventSourceURL  string                    `json:"eventSourceUrl"`
	State           string                    `json:"state"`
}

// SessionAccount describes one account in the session.
type SessionAccount struct {
	Name                string         `json:"name"`
	IsPersonal          bool           `json:"isPersonal"`
	IsReadOnly          bool           `json:"isReadOnly"`
	AccountCapabilities map[string]any `json:"accountCapabilities"`
}

var emailSortOptions = []string{"receivedAt", "sentAt", "size", "from", "to", "subject", "hasKeyword", "allInThreadHaveKeyword", "someInThreadHaveKeyword"}

func coreCapability() map[string]any {
	return map[string]any{
		"maxSizeUpload":         MaxSizeUpload,
		"maxConcurrentUpload":   MaxConcurrentUpload,
		"maxSizeRequest":        MaxSizeRequest,
		"maxConcurrentRequests": MaxConcurrentRequests,
		"maxCallsInRequest":     MaxCallsInRequest,
		"maxObjectsInGet":       MaxObjectsInGet,
		"maxObjectsInSet":       MaxObjectsInSet,
		"collationAlgorithms":   []string{"i;unicode-casemap"},
	}
}

// Session builds the session for the caller's account.
func (s *Server) Session(ctx context.Context, acct Account) (*Session, error) {
	st, err := s.Store.State(ctx)
	if err != nil {
		return nil, err
	}
	s.epoch = st.Epoch
	id := s.publicID(acct)
	return &Session{
		Capabilities: map[string]any{CapCore: coreCapability(), CapMail: map[string]any{}},
		Accounts: map[string]SessionAccount{id: {
			Name:       acct.Username,
			IsPersonal: true,
			IsReadOnly: false,
			AccountCapabilities: map[string]any{CapMail: map[string]any{
				"maxMailboxesPerEmail":       MaxMailboxesPerEmail,
				"maxMailboxDepth":            maxMailboxDepth,
				"maxSizeMailboxName":         maxMailboxNameSize,
				"maxSizeAttachmentsPerEmail": 20 << 20,
				"emailQuerySortOptions":      emailSortOptions,
				"mayCreateTopLevelMailbox":   true,
			}},
		}},
		PrimaryAccounts: map[string]string{CapMail: id},
		Username:        acct.Username,
		APIURL:          s.BaseURL + "/jmap",
		DownloadURL:     s.BaseURL + "/download/{accountId}/{blobId}/{name}?type={type}",
		UploadURL:       s.BaseURL + "/upload/{accountId}",
		EventSourceURL:  s.BaseURL + "/eventsource?types={types}&closeafter={closeafter}&ping={ping}",
		State:           s.sessionState(acct, st),
	}, nil
}

// sessionState changes when anything in the session object would.
func (s *Server) sessionState(acct Account, st State) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("v1\x00%s\x00%d\x00%s\x00%s", acct.InternalID, st.Epoch, acct.Username, s.BaseURL)))
	return hex.EncodeToString(sum[:8])
}

// utcDate formats a JMAP UTCDate.
func utcDate(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
