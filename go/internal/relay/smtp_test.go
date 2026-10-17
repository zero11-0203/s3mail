package relay

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// server is a scripted SMTP server.
type server struct {
	implicitTLS bool
	noSTARTTLS  bool
	rcpt        map[string]string // address -> reply, default 250
	data        string            // reply to DATA, default 354
	final       string            // final reply, default 250; "drop" closes
	tlsConf     *tls.Config

	mu       sync.Mutex
	commands []string
	content  string
}

func (s *server) log(c string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, c)
}

func (s *server) saw(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (s *server) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if s.implicitTLS {
		conn = tls.Server(conn, s.tlsConf)
	}
	rd, w := bufio.NewReader(conn), conn
	say := func(l string) { _, _ = w.Write([]byte(l + "\r\n")) }
	say("220 relay.example.com ESMTP")
	tlsOn := s.implicitTLS
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		s.log(line)
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			say("250-relay.example.com")
			if !tlsOn && !s.noSTARTTLS {
				say("250-STARTTLS")
			}
			say("250-8BITMIME")
			say("250-SIZE 52428800")
			say("250 AUTH PLAIN LOGIN")
		case up == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(conn, s.tlsConf)
			if tc.Handshake() != nil {
				return
			}
			conn, tlsOn = tc, true
			rd, w = bufio.NewReader(tc), tc
		case strings.HasPrefix(up, "AUTH PLAIN"):
			say("235 2.7.0 ok")
		case strings.HasPrefix(up, "MAIL FROM"):
			say("250 2.1.0 ok")
		case strings.HasPrefix(up, "RCPT TO:<"):
			addr := strings.TrimSuffix(strings.TrimPrefix(line, "RCPT TO:<"), ">")
			if r, ok := s.rcpt[addr]; ok {
				say(r)
			} else {
				say("250 2.1.5 ok")
			}
		case up == "RSET":
			say("250 ok")
		case up == "DATA":
			if s.data != "" {
				say(s.data)
				continue
			}
			say("354 go")
			var b strings.Builder
			for {
				l, err := rd.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.content = b.String()
			s.mu.Unlock()
			switch s.final {
			case "drop":
				return
			case "":
				say("250 2.0.0 OK id=1abcde-000001-AB")
			default:
				say(s.final)
			}
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("502 unknown")
		}
	}
}

func certs(t *testing.T) (*tls.Config, *x509.CertPool) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay.example.com"}, DNSNames: []string{"relay.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}

func run(t *testing.T, s *server, msg string, handoff func(context.Context) error) Result {
	t.Helper()
	srvConf, pool := certs(t)
	s.tlsConf = srvConf
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	r := &SMTP{
		Host: "relay.example.com", Port: 587, ImplicitTLS: s.implicitTLS, Username: "u", Password: "p", HeloName: "example.com",
		Timeout: 5 * time.Second, TLSConfig: &tls.Config{RootCAs: pool},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
		},
	}
	return r.Submit(context.Background(), Message{
		Envelope: Envelope{MailFrom: "me@example.com", RcptTo: []string{"a@example.net", "b@example.net"}},
		Body:     strings.NewReader(msg), Handoff: handoff,
	})
}

const msg = "From: me@example.com\r\nTo: a@example.net\r\nSubject: hi\r\n\r\n.leading dot\r\nbody\r\n"

func TestSMTPAccepted(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		s := &server{implicitTLS: implicit}
		handed := false
		res := run(t, s, msg, func(context.Context) error { handed = true; return nil })
		if res.Outcome != Accepted || res.Reply != "250 2.0.0 OK id=1abcde-000001-AB" || res.RelayID != "1abcde-000001-AB" || !handed {
			t.Fatalf("implicit=%v: %+v", implicit, res)
		}
		if !strings.Contains(s.content, "\r\n..leading dot\r\n") {
			t.Errorf("dot stuffing: %q", s.content)
		}
		if implicit == s.saw("STARTTLS") {
			t.Errorf("implicit=%v STARTTLS use", implicit)
		}
	}
}

func TestSMTPRefusesPlaintext(t *testing.T) {
	s := &server{noSTARTTLS: true}
	res := run(t, s, msg, nil)
	if res.Outcome != NotAccepted || res.Stage != "starttls" || s.saw("AUTH") || s.saw("MAIL") {
		t.Fatalf("%+v %v", res, s.commands)
	}
}

func TestSMTPRecipientRefusedAbortsBeforeData(t *testing.T) {
	s := &server{rcpt: map[string]string{"b@example.net": "550 5.1.1 no such user"}}
	handed := false
	res := run(t, s, msg, func(context.Context) error { handed = true; return nil })
	if res.Outcome != Rejected || res.Stage != "rcpt" || handed || s.saw("DATA") || !s.saw("RSET") {
		t.Fatalf("%+v %v", res, s.commands)
	}
	if res.Recipients["a@example.net"] != "250 2.1.5 ok" || res.Recipients["b@example.net"] != "550 5.1.1 no such user" {
		t.Errorf("recipients %+v", res.Recipients)
	}
	s = &server{rcpt: map[string]string{"a@example.net": "451 4.3.0 try later"}}
	if res := run(t, s, msg, nil); res.Outcome != NotAccepted || res.Stage != "rcpt" {
		t.Errorf("temporary rcpt: %+v", res)
	}
}

func TestSMTPHandoffFailureSendsNothing(t *testing.T) {
	s := &server{}
	res := run(t, s, msg, func(context.Context) error { return errors.New("lease lost") })
	if res.Outcome != NotAccepted || res.Stage != "handoff" || s.saw("DATA") {
		t.Fatalf("%+v %v", res, s.commands)
	}
}

func TestSMTPFinalReplies(t *testing.T) {
	cases := []struct {
		s     *server
		want  Outcome
		stage string
	}{
		{&server{data: "554 5.3.4 no"}, Rejected, "data"},
		{&server{final: "452 4.3.1 full"}, NotAccepted, "final"},
		{&server{final: "550 5.7.1 spam"}, Rejected, "final"},
		// Content sent, no reply: acceptance is unknown.
		{&server{final: "drop"}, Unknown, "final"},
	}
	for _, c := range cases {
		if res := run(t, c.s, msg, nil); res.Outcome != c.want || res.Stage != c.stage {
			t.Errorf("%+v: %+v", c.s, res)
		}
	}
}

func TestStripBcc(t *testing.T) {
	in := "From: a@example.com\r\nBcc: secret@example.net,\r\n other@example.net\r\nbcc:x@example.net\r\nTo: b@example.net\r\n\r\nBcc: body line stays\r\n"
	want := "From: a@example.com\r\nTo: b@example.net\r\n\r\nBcc: body line stays\r\n"
	if got := string(StripBcc([]byte(in))); got != want {
		t.Errorf("got %q", got)
	}
}
