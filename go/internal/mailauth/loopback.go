package mailauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Listen binds the loopback address of the callback URL. Cognito matches
// redirect URIs exactly, so the port is fixed by the app client config.
func Listen(callbackURL string) (net.Listener, *url.URL, error) {
	u, err := url.Parse(callbackURL)
	if err != nil {
		return nil, nil, err
	}
	if u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") || u.Port() == "" || u.Port() == "0" {
		return nil, nil, fmt.Errorf("callback %q must be http://localhost:<port>/<path>", callbackURL)
	}
	// Bind only the IPv4 loopback interface, never all interfaces.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", u.Port()))
	if err != nil {
		return nil, nil, fmt.Errorf("listen on callback port %s: %w", u.Port(), err)
	}
	return ln, u, nil
}

// AwaitCode serves one OAuth redirect and returns its authorization code
// after checking state. Other paths get 404.
func AwaitCode(ctx context.Context, ln net.Listener, callback *url.URL, state string) (string, error) {
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(callback.Path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		// A redirect without our state is not ours: reject it and keep
		// waiting, so a forged request can neither complete nor cancel login.
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintln(w, "mail-auth: unexpected redirect ignored.")
			return
		}
		var res result
		switch {
		case q.Get("error") != "":
			res.err = fmt.Errorf("login failed: %s", q.Get("error"))
		case q.Get("code") == "":
			res.err = errors.New("redirect has no code")
		default:
			res.code = q.Get("code")
		}
		if res.err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintln(w, "mail-auth: login did not complete. Return to the terminal.")
		} else {
			_, _ = fmt.Fprintln(w, "mail-auth: login complete. You can close this tab.")
		}
		select {
		case done <- res:
		default:
		}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		// Let the browser receive its response before closing.
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-done:
		return res.code, res.err
	}
}
