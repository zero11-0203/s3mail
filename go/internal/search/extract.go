package search

import (
	"strings"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/mailparse"
)

// Bounds on what one message contributes to the index.
const (
	maxIndexedBody  = 1 << 20  // visible text indexed per message
	maxStoredBody   = 64 << 10 // text kept for snippets
	maxHeaderTokens = 16 << 10
)

// Doc is one Email's indexed projection.
type Doc struct {
	EmailID, ThreadID string
	ReceivedAt        time.Time
	From, To, Cc, Bcc string
	Subject, Body     string
	AttachmentNames   string
	HasAttachment     bool
}

// Extract builds the indexed projection of a raw message: decoded address
// headers (names and addresses), the subject, the visible text of the
// text body parts and the attachment names.
func Extract(emailID, threadID string, receivedAt time.Time, raw []byte) Doc {
	msg := mailparse.Parse(raw)
	h := func(name string) string {
		var vals []string
		for _, v := range msg.Headers.All(name) {
			vals = append(vals, mailparse.AsText(v))
		}
		return capBytes(strings.Join(vals, ", "), maxHeaderTokens)
	}
	d := Doc{
		EmailID: emailID, ThreadID: threadID, ReceivedAt: receivedAt,
		From: h("From"), To: h("To"), Cc: h("Cc"), Bcc: h("Bcc"), Subject: h("Subject"),
	}
	b := msg.BodyLists()
	var body strings.Builder
	for _, p := range b.Text {
		if body.Len() >= maxIndexedBody {
			break
		}
		if strings.HasPrefix(p.Type, "text/") {
			body.WriteString(p.VisibleText(maxIndexedBody - body.Len()))
			body.WriteString("\n")
		}
	}
	// HTML-only messages carry their text in htmlBody.
	if strings.TrimSpace(body.String()) == "" {
		for _, p := range b.HTML {
			body.WriteString(p.VisibleText(maxIndexedBody))
		}
	}
	d.Body = body.String()
	var names []string
	for _, p := range b.Attachments {
		if p.Name != "" {
			names = append(names, p.Name)
		}
	}
	d.AttachmentNames = strings.Join(names, " ")
	d.HasAttachment = len(b.Attachments) > 0
	return d
}

func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
