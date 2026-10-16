package auth

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// ErrUnauthorized is the exact error API Gateway maps to a 401 response
// for TOKEN authorizers.
var ErrUnauthorized = errors.New("Unauthorized")

// Authorizer is an API Gateway TOKEN authorizer. Results must not be
// cached (TTL 0), so the policy names only the invoked method.
type Authorizer struct {
	Verifier *Verifier
}

// Handle validates the Authorization header value and allows exactly the
// requested method. Handlers still enforce per-operation scopes and the
// subject-to-account mapping (docs/specs/security.md#scopes).
func (a *Authorizer) Handle(ctx context.Context, req events.APIGatewayCustomAuthorizerRequest) (events.APIGatewayCustomAuthorizerResponse, error) {
	token, err := BearerToken(req.AuthorizationToken)
	if err == nil {
		var c Claims
		c, err = a.Verifier.Verify(ctx, token)
		if err == nil {
			return allow(c, req.MethodArn), nil
		}
	}
	// Log the reason without the token itself.
	log.Printf("authorizer: rejected: %v", err)
	return events.APIGatewayCustomAuthorizerResponse{}, ErrUnauthorized
}

func allow(c Claims, methodArn string) events.APIGatewayCustomAuthorizerResponse {
	return events.APIGatewayCustomAuthorizerResponse{
		PrincipalID: c.Subject,
		PolicyDocument: events.APIGatewayCustomAuthorizerPolicy{
			Version: "2012-10-17",
			Statement: []events.IAMPolicyStatement{{
				Action:   []string{"execute-api:Invoke"},
				Effect:   "Allow",
				Resource: []string{methodArn},
			}},
		},
		Context: map[string]any{
			"sub":      c.Subject,
			"username": c.Username,
			"clientId": c.ClientID,
			"scope":    strings.Join(c.Scopes, " "),
			// Push closes its stream when the token expires.
			"exp": c.Expires.Unix(),
		},
	}
}
