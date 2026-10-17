// Package smtp2go translates SMTP2GO webhook notifications into relay
// delivery events (docs/specs/mail-flow.md#outbound-relay-interface). The
// webhook is configured for JSON output; field names follow SMTP2GO's
// webhook reference.
package smtp2go

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/relay"
)

// Relay is the adapter's relay name.
const Relay = "smtp2go"

// notification is one webhook event; SMTP2GO sends string values.
type notification struct {
	Event    string `json:"event"`
	Time     string `json:"time"`
	SendTime string `json:"sendtime"`
	Rcpt     string `json:"rcpt"`
	EmailID  string `json:"email_id"`
	ID       string `json:"id"`
	Bounce   string `json:"bounce"`
	Host     string `json:"host"`
	Message  string `json:"message"`
	Context  string `json:"context"`
}

// ErrMalformed means the body is not a JSON webhook notification.
var ErrMalformed = errors.New("malformed SMTP2GO notification")

// Parse returns the delivery events in a webhook body: one object or an
// array of objects. Events that carry no delivery state (processed, open,
// click, unsubscribe, resubscribe) are skipped.
func Parse(body []byte) ([]relay.DeliveryEvent, error) {
	body = bytes.TrimSpace(body)
	var list []notification
	switch {
	case len(body) > 0 && body[0] == '[':
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
	case len(body) > 0 && body[0] == '{':
		var n notification
		if err := json.Unmarshal(body, &n); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		list = []notification{n}
	default:
		return nil, ErrMalformed
	}
	var out []relay.DeliveryEvent
	for _, n := range list {
		if n.Event == "" {
			return nil, fmt.Errorf("%w: no event", ErrMalformed)
		}
		ev, ok := n.event()
		if ok {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (n notification) event() (relay.DeliveryEvent, bool) {
	var delivered string
	reply := strings.TrimSpace(strings.Join(nonEmpty(n.Message, n.Context), " "))
	switch n.Event {
	case "delivered":
		delivered = "yes"
	case "bounce":
		// A soft bounce is a temporary failure SMTP2GO may still retry.
		delivered = "no"
		if strings.EqualFold(n.Bounce, "soft") {
			delivered = "queued"
		}
		if n.Host != "" {
			reply = strings.TrimSpace(reply + " (" + n.Host + ")")
		}
	case "reject":
		// Not sent at all, for example a suppressed recipient.
		delivered = "no"
	case "spam":
		// A complaint: the message was delivered.
		delivered = "yes"
		reply = strings.TrimSpace("recipient reported spam " + reply)
	default:
		return relay.DeliveryEvent{}, false
	}
	if n.EmailID == "" || n.Rcpt == "" {
		return relay.DeliveryEvent{}, false
	}
	// No per-event id is documented, so the id is a canonical composite
	// of the fields that identify one report.
	sum := sha256.Sum256([]byte(strings.Join([]string{n.ID, n.Event, n.EmailID, strings.ToLower(n.Rcpt), n.Time, n.Bounce}, "\x00")))
	return relay.DeliveryEvent{
		Relay: Relay, ID: hex.EncodeToString(sum[:16]), RelayID: n.EmailID, Recipient: n.Rcpt,
		Delivered: delivered, Reply: reply, At: parseTime(n.Time),
	}, true
}

func nonEmpty(v ...string) []string {
	var out []string
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseTime accepts the timestamp forms SMTP2GO is known to use; an
// unknown form yields the zero time, which only affects display.
func parseTime(v string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
