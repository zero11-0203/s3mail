package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// SMTP is the authenticated SMTP submission adapter (SMTP2GO initially).
// TLS is required: implicit TLS, or STARTTLS which must be offered; it
// never falls back to plaintext. Certificates and the host name are
// verified.
type SMTP struct {
	Host string
	Port int
	// ImplicitTLS selects TLS from connect (465); otherwise STARTTLS.
	ImplicitTLS bool
	Username    string
	Password    string
	// HeloName is the EHLO name (the sending domain).
	HeloName string
	// Timeout bounds the whole transaction (60 s by default).
	Timeout time.Duration
	// TLSConfig overrides the TLS settings (tests supply RootCAs).
	TLSConfig *tls.Config
	// Dial overrides the dialer (tests).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

type smtpConn struct {
	conn net.Conn
	tp   *textproto.Conn
}

func (c *smtpConn) cmd(expect int, format string, args ...any) (int, string, error) {
	id, err := c.tp.Cmd(format, args...)
	if err != nil {
		return 0, "", err
	}
	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)
	return c.tp.ReadResponse(expect)
}

// reply formats a server reply for storage.
func reply(code int, msg string) string {
	return strconv.Itoa(code) + " " + strings.ReplaceAll(msg, "\n", " ")
}

// classify maps a negative reply: 5xx is permanent, anything else is
// retryable. Only call it where no message content was transmitted.
func classify(code int) Outcome {
	if code >= 500 && code < 600 {
		return Rejected
	}
	return NotAccepted
}

func (r *SMTP) tlsConfig() *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if r.TLSConfig != nil {
		cfg = r.TLSConfig.Clone()
	}
	cfg.ServerName = r.Host
	cfg.InsecureSkipVerify = false
	return cfg
}

// Submit runs one SMTP transaction.
func (r *SMTP) Submit(ctx context.Context, m Message) Result {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res := Result{Recipients: map[string]string{}}
	fail := func(stage string, o Outcome, why string) Result {
		res.Outcome, res.Stage, res.Reply = o, stage, why
		return res
	}
	body, err := io.ReadAll(m.Body)
	if err != nil {
		return fail("read", NotAccepted, err.Error())
	}

	dial := r.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(r.Host, strconv.Itoa(r.Port)))
	if err != nil {
		return fail("connect", NotAccepted, err.Error())
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// Cancellation closes the connection, unblocking any read or write.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if r.ImplicitTLS {
		tc := tls.Client(conn, r.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			return fail("tls", NotAccepted, err.Error())
		}
		conn = tc
	}
	c := &smtpConn{conn: conn, tp: textproto.NewConn(conn)}
	if code, msg, err := c.tp.ReadResponse(220); err != nil {
		return fail("greeting", NotAccepted, describe(code, msg, err))
	}
	ext, res2 := c.ehlo(r.HeloName)
	if res2 != nil {
		return fail("ehlo", NotAccepted, *res2)
	}
	if !r.ImplicitTLS {
		if _, ok := ext["STARTTLS"]; !ok {
			return fail("starttls", NotAccepted, "server does not offer STARTTLS; refusing plaintext")
		}
		if code, msg, err := c.cmd(220, "STARTTLS"); err != nil {
			return fail("starttls", NotAccepted, describe(code, msg, err))
		}
		tc := tls.Client(conn, r.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			return fail("tls", NotAccepted, err.Error())
		}
		conn = tc
		c = &smtpConn{conn: conn, tp: textproto.NewConn(conn)}
		if ext, res2 = c.ehlo(r.HeloName); res2 != nil {
			return fail("ehlo", NotAccepted, *res2)
		}
	}
	defer func() { _, _, _ = c.cmd(221, "QUIT") }()

	if !strings.Contains(" "+ext["AUTH"]+" ", " PLAIN ") {
		return fail("auth", NotAccepted, "server does not offer AUTH PLAIN")
	}
	cred := base64.StdEncoding.EncodeToString([]byte("\x00" + r.Username + "\x00" + r.Password))
	if code, msg, err := c.cmd(235, "AUTH PLAIN %s", cred); err != nil {
		// Credentials are configuration: retryable once they are fixed.
		return fail("auth", NotAccepted, describe(code, msg, err))
	}

	params := ""
	if _, ok := ext["8BITMIME"]; ok && has8bit(body) {
		params += " BODY=8BITMIME"
	}
	if _, ok := ext["SIZE"]; ok {
		params += " SIZE=" + strconv.Itoa(len(body))
	}
	if code, msg, err := c.cmd(250, "MAIL FROM:<%s>%s", m.Envelope.MailFrom, params); err != nil {
		return fail("mail", classifyErr(code, err), describe(code, msg, err))
	}
	worst := 0
	for _, rcpt := range m.Envelope.RcptTo {
		code, msg, err := c.cmd(25, "RCPT TO:<%s>", rcpt)
		if code == 0 && err != nil {
			return fail("rcpt", NotAccepted, err.Error())
		}
		res.Recipients[rcpt] = reply(code, msg)
		if err != nil && code > worst {
			worst = code
		}
	}
	if worst > 0 {
		// Never deliver to a subset: abort before DATA.
		_, _, _ = c.cmd(250, "RSET")
		return fail("rcpt", classify(worst), "a recipient was refused")
	}

	if m.Handoff != nil {
		if err := m.Handoff(ctx); err != nil {
			_, _, _ = c.cmd(250, "RSET")
			return fail("handoff", NotAccepted, err.Error())
		}
	}
	// No content has been sent until the server answers 354.
	if code, msg, err := c.cmd(354, "DATA"); err != nil {
		return fail("data", classifyErr(code, err), describe(code, msg, err))
	}
	w := c.tp.DotWriter()
	if _, err := w.Write(body); err != nil {
		return fail("content", Unknown, err.Error())
	}
	if err := w.Close(); err != nil {
		return fail("content", Unknown, err.Error())
	}
	code, msg, err := c.tp.ReadResponse(250)
	switch {
	case err == nil:
		res.Outcome, res.Stage, res.Reply = Accepted, "final", reply(code, msg)
		return res
	case code == 0:
		// The message was transmitted but no reply arrived.
		return fail("final", Unknown, err.Error())
	}
	return fail("final", classify(code), reply(code, msg))
}

func (c *smtpConn) ehlo(name string) (map[string]string, *string) {
	code, msg, err := c.cmd(250, "EHLO %s", name)
	if err != nil {
		d := describe(code, msg, err)
		return nil, &d
	}
	ext := map[string]string{}
	for _, line := range strings.Split(msg, "\n")[1:] {
		k, v, _ := strings.Cut(line, " ")
		ext[strings.ToUpper(k)] = strings.ToUpper(v)
	}
	return ext, nil
}

func classifyErr(code int, err error) Outcome {
	var pe *textproto.Error
	if errors.As(err, &pe) {
		return classify(pe.Code)
	}
	if code != 0 {
		return classify(code)
	}
	return NotAccepted
}

func describe(code int, msg string, err error) string {
	if code != 0 {
		return reply(code, msg)
	}
	return fmt.Sprint(err)
}

func has8bit(b []byte) bool {
	return bytes.ContainsFunc(b, func(r rune) bool { return r >= 0x80 })
}
