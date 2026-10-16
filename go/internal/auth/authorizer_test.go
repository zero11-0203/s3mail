package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestAuthorizer(t *testing.T) {
	var fetches atomic.Int32
	a := &Authorizer{Verifier: newVerifier(t, &fetches)}
	const arn = "arn:aws:execute-api:us-east-1:111122223333:abc123/spike/GET/stream"
	valid := sign(keyA, map[string]any{"alg": "RS256", "kid": "a"}, accessClaims())

	resp, err := a.Handle(context.Background(), events.APIGatewayCustomAuthorizerRequest{
		Type: "TOKEN", AuthorizationToken: "Bearer " + valid, MethodArn: arn,
	})
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	st := resp.PolicyDocument.Statement
	if resp.PrincipalID != "owner-sub" || len(st) != 1 || st[0].Effect != "Allow" ||
		len(st[0].Resource) != 1 || st[0].Resource[0] != arn {
		t.Fatalf("policy = %+v", resp)
	}

	for name, header := range map[string]string{
		"empty":     "",
		"basic":     "Basic dXNlcjpwYXNz",
		"raw token": valid,
		"bad token": "Bearer a.b.c",
	} {
		_, err := a.Handle(context.Background(), events.APIGatewayCustomAuthorizerRequest{
			Type: "TOKEN", AuthorizationToken: header, MethodArn: arn,
		})
		if !errors.Is(err, ErrUnauthorized) || err.Error() != "Unauthorized" {
			t.Errorf("%s: err = %v, want exactly Unauthorized", name, err)
		}
	}
}
