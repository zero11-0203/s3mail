// Package buildinfo reports the deployed release version of a binary.
//
// Release builds set Version with
//
//	-ldflags "-X github.com/zero11-0203/s3mail/go/internal/buildinfo.Version=<commit SHA>"
//
// so Lambdas and CLIs can identify their release, for example in the
// canary's deployment-version record.
package buildinfo

// Version is the release identifier injected at build time.
var Version = ""

// String returns Version, or "dev" for builds without an injected version.
func String() string {
	if Version == "" {
		return "dev"
	}
	return Version
}
