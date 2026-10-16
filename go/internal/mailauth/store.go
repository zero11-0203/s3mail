package mailauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// ErrNoSession means login has not been run (or logout removed it).
var ErrNoSession = errors.New("no session; run mail-auth login")

// Store keeps the session in the OS keychain.
type Store interface {
	Load() (Session, error)
	Save(Session) error
	Delete() error
}

// Keychain stores the session as a macOS generic password through
// /usr/bin/security. Secrets are written on stdin (security -i), never as
// command-line arguments, so they do not appear in the process list.
type Keychain struct {
	Service string // e.g. "s3mail-mail-auth"
	Account string // the Cognito client ID, so pools do not collide
}

func (k Keychain) supported() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("keychain storage is implemented for macOS only (GOOS=%s)", runtime.GOOS)
	}
	return nil
}

func (k Keychain) Load() (Session, error) {
	if err := k.supported(); err != nil {
		return Session{}, err
	}
	out, err := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", k.Service, "-a", k.Account, "-w").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 44 { // errSecItemNotFound
			return Session{}, ErrNoSession
		}
		return Session{}, fmt.Errorf("read keychain: %w", err)
	}
	var s Session
	if err := json.Unmarshal(bytes.TrimSpace(out), &s); err != nil {
		return Session{}, fmt.Errorf("keychain item is not a session: %w", err)
	}
	return s, nil
}

func (k Keychain) Save(s Session) error {
	if err := k.supported(); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	// -U updates an existing item. strconv.Quote keeps the value one
	// argument for security's interactive command parser.
	cmd := exec.Command("/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n",
		strconv.Quote(k.Service), strconv.Quote(k.Account), strconv.Quote(string(b))))
	if out, err := cmd.CombinedOutput(); err != nil || bytes.Contains(out, []byte("error")) {
		return fmt.Errorf("write keychain: %v %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func (k Keychain) Delete() error {
	if err := k.supported(); err != nil {
		return err
	}
	err := exec.Command("/usr/bin/security", "delete-generic-password",
		"-s", k.Service, "-a", k.Account).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 44 {
		return nil
	}
	return err
}

// Lock serializes token refreshes across concurrent mail-auth processes, so
// two callers never race to rotate the same refresh token.
func Lock(dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "mail-auth.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
