// Package relay is the outbound relay interface and its adapters
// (docs/specs/mail-flow.md#outbound-relay-interface). Nothing outside the
// adapters knows which relay is in use.
package relay

import (
	"bytes"
	"context"
	"io"
	"strings"
)

// Envelope is the SMTP envelope of one submission.
type Envelope struct {
	MailFrom string
	RcptTo   []string
}

// Outcome classifies an attempt for the at-most-once send protocol.
type Outcome int

const (
	// Accepted: the relay took responsibility for the message.
	Accepted Outcome = iota + 1
	// NotAccepted: proven not accepted, and retrying may succeed.
	NotAccepted
	// Rejected: proven not accepted, permanently.
	Rejected
	// Unknown: the relay may have accepted the message. Never resend
	// automatically.
	Unknown
)

func (o Outcome) String() string {
	switch o {
	case Accepted:
		return "accepted"
	case NotAccepted:
		return "not-accepted"
	case Rejected:
		return "rejected"
	case Unknown:
		return "unknown"
	}
	return "invalid"
}

// Message is one attempt's input. Handoff runs immediately before any
// operation that could transmit the message; if it fails the attempt
// aborts without transmitting and is NotAccepted with Stage "handoff".
type Message struct {
	Envelope Envelope
	Body     io.Reader
	Handoff  func(ctx context.Context) error
}

// Result is the outcome of one attempt. Reply is the reply that decided
// it (or the error text); Recipients holds each RCPT reply.
type Result struct {
	Outcome    Outcome
	Stage      string
	Reply      string
	Recipients map[string]string
}

// Relay submits one message.
type Relay interface {
	Submit(ctx context.Context, m Message) Result
}

// StripBcc removes Bcc header fields (with continuation lines) from raw
// MIME and leaves every other byte unchanged.
func StripBcc(raw []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(raw))
	rest := raw
	skipping := false
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		line := rest
		if i >= 0 {
			line = rest[:i+1]
		}
		rest = rest[len(line):]
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 {
			// End of the header section.
			out.Write(line)
			out.Write(rest)
			return out.Bytes()
		}
		if line[0] == ' ' || line[0] == '\t' {
			if !skipping {
				out.Write(line)
			}
			continue
		}
		name, _, _ := strings.Cut(string(trimmed), ":")
		skipping = strings.EqualFold(strings.TrimSpace(name), "bcc")
		if !skipping {
			out.Write(line)
		}
	}
	return out.Bytes()
}
