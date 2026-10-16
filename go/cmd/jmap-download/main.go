// Command jmap-download serves JMAP blob downloads (docs/specs/jmap.md#limits-and-blobs).
package main

import (
	"github.com/zero11-0203/s3mail/go/internal/jmap"
	"github.com/zero11-0203/s3mail/go/internal/jmapcmd"
)

func main() { jmapcmd.Start("jmap-download", (*jmap.Server).HandleDownload) }
