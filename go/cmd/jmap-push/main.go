// Command jmap-push serves the JMAP EventSource push stream (docs/specs/jmap.md#push).
package main

import (
	"github.com/zero11-0203/s3mail/go/internal/jmap"
	"github.com/zero11-0203/s3mail/go/internal/jmapcmd"
)

func main() { jmapcmd.Start("jmap-push", (*jmap.Server).HandleEventSource) }
