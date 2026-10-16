// Command spike-stream is the Phase 0 response-streaming proof Lambda.
package main

import (
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/zero11-0203/s3mail/go/internal/spike"
)

func main() {
	lambda.Start(spike.Handle)
}
