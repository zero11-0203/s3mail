// Command authorizer is the API Gateway Lambda authorizer that applies the
// shared bearer-token contract to every JMAP route.
//
// Environment: ISSUER (Cognito issuer URL) and CLIENT_IDS (comma-separated
// allowlisted app client IDs).
package main

import (
	"log"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/zero11-0203/s3mail/go/internal/auth"
)

func main() {
	issuer := os.Getenv("ISSUER")
	var clients []string
	for _, c := range strings.Split(os.Getenv("CLIENT_IDS"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			clients = append(clients, c)
		}
	}
	if !strings.HasPrefix(issuer, "https://") || len(clients) == 0 {
		log.Fatal("ISSUER (https URL) and CLIENT_IDS are required")
	}
	a := &auth.Authorizer{Verifier: &auth.Verifier{
		Issuer:    issuer,
		ClientIDs: clients,
		Keys:      &auth.JWKS{URL: issuer + "/.well-known/jwks.json"},
	}}
	lambda.Start(a.Handle)
}
