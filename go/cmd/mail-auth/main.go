// Command mail-auth obtains OAuth tokens for the s3mail JMAP API.
//
//	mail-auth login    sign in through the browser (code + PKCE) and store the session
//	mail-auth token    print a valid access token (for aerc's source-cred-cmd)
//	mail-auth status   show whether a session exists and when its access token expires
//	mail-auth logout   revoke the refresh token and delete the stored session
//
// Configuration is the JSON from `terraform output -json auth`, read from
// -config (default ~/.config/s3mail/auth.json).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/buildinfo"
	"github.com/zero11-0203/s3mail/go/internal/mailauth"
)

// refreshMargin keeps printed tokens usable for at least a minute.
const refreshMargin = time.Minute

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mail-auth:", err)
		os.Exit(1)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "s3mail")
	fs := flag.NewFlagSet("mail-auth", flag.ContinueOnError)
	configPath := fs.String("config", filepath.Join(dir, "auth.json"), "auth config (terraform output -json auth)")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: mail-auth [-config file] login|token|status|logout|version")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("one command required")
	}
	if fs.Arg(0) == "version" {
		fmt.Println(buildinfo.String())
		return nil
	}

	cfg, err := mailauth.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	client := &mailauth.Client{Config: cfg}
	store := mailauth.Keychain{Service: "s3mail-mail-auth", Account: cfg.TerminalClientID}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch fs.Arg(0) {
	case "login":
		return login(ctx, client, store)
	case "token":
		return token(ctx, client, store, dir)
	case "status":
		s, err := store.Load()
		if err != nil {
			return err
		}
		fmt.Printf("session stored; access token valid until %s\n", s.ExpiresAt.Local().Format(time.RFC3339))
		return nil
	case "logout":
		s, err := store.Load()
		if errors.Is(err, mailauth.ErrNoSession) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := client.Revoke(ctx, s.RefreshToken); err != nil {
			fmt.Fprintln(os.Stderr, "mail-auth: warning:", err)
		}
		return store.Delete()
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", fs.Arg(0))
	}
}

func login(ctx context.Context, c *mailauth.Client, store mailauth.Store) error {
	ln, callback, err := mailauth.Listen(c.Config.CallbackURL)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	pkce, err := mailauth.NewPKCE()
	if err != nil {
		return err
	}
	state, err := mailauth.NewPKCE() // reuse the random generator for state
	if err != nil {
		return err
	}
	authURL := c.Config.AuthorizeURL(state.Verifier, pkce)

	fmt.Fprintln(os.Stderr, "Opening the s3mail login page. If it does not open, visit:")
	fmt.Fprintln(os.Stderr, authURL)
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", authURL).Start()
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	code, err := mailauth.AwaitCode(waitCtx, ln, callback, state.Verifier)
	if err != nil {
		return err
	}
	s, err := c.Exchange(ctx, code, pkce.Verifier)
	if err != nil {
		return err
	}
	if err := store.Save(s); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Logged in; session stored in the keychain.")
	return nil
}

func token(ctx context.Context, c *mailauth.Client, store mailauth.Store, lockDir string) error {
	unlock, err := mailauth.Lock(lockDir)
	if err != nil {
		return err
	}
	defer unlock()

	s, err := store.Load()
	if err != nil {
		return err
	}
	if !s.Valid(time.Now(), refreshMargin) {
		next, err := c.Refresh(ctx, s)
		if err != nil {
			return err
		}
		// Save before printing: the old refresh token is rotated out.
		if err := store.Save(next); err != nil {
			return err
		}
		s = next
	}
	fmt.Println(s.AccessToken)
	return nil
}
