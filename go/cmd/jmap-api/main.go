// Command jmap-api serves the JMAP session and API endpoints (docs/specs/jmap.md).
package main

import (
	"github.com/zero11-0203/s3mail/go/internal/jmap"
	"github.com/zero11-0203/s3mail/go/internal/jmapcmd"
)

func main() { jmapcmd.Start("jmap-api", (*jmap.Server).HandleAPI) }
