package mailauth

import (
	"errors"
	"os"
	"runtime"
	"testing"
	"time"
)

// TestKeychainRoundTrip uses the real macOS keychain with a throwaway item.
// Opt in with MAILAUTH_KEYCHAIN_TEST=1.
func TestKeychainRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("MAILAUTH_KEYCHAIN_TEST") != "1" {
		t.Skip("set MAILAUTH_KEYCHAIN_TEST=1 on macOS to run")
	}
	k := Keychain{Service: "s3mail-mail-auth-test", Account: "roundtrip"}
	t.Cleanup(func() { _ = k.Delete() })

	want := Session{
		RefreshToken: `rt "quoted" \back\slash $HOME 'single' ` + "tab\tend",
		AccessToken:  "eyJ.payload.sig",
		ExpiresAt:    time.Unix(1_800_000_000, 0).UTC(),
	}
	for range 2 { // second save exercises -U update
		if err := k.Save(want); err != nil {
			t.Fatal(err)
		}
	}
	got, err := k.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != want.RefreshToken || got.AccessToken != want.AccessToken || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	if err := k.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Load(); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after delete: %v", err)
	}
}
