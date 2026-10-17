package jmapcmd

import (
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// allowOrigin lets the one registered browser origin read a response
// (docs/specs/security.md#shared-endpoint-authorization). origin is
// CORS_ORIGIN; empty disables
// CORS. Preflights and gateway errors get their headers from API Gateway.
func allowOrigin(origin string, req events.APIGatewayProxyRequest, resp *events.APIGatewayProxyStreamingResponse) {
	if origin == "" || resp == nil {
		return
	}
	if resp.Headers == nil {
		resp.Headers = map[string]string{}
	}
	// Caches must not reuse one origin's response for another.
	if v := resp.Headers["Vary"]; v == "" {
		resp.Headers["Vary"] = "Origin"
	} else {
		resp.Headers["Vary"] = v + ", Origin"
	}
	for k, v := range req.Headers {
		if strings.EqualFold(k, "Origin") && v == origin {
			resp.Headers["Access-Control-Allow-Origin"] = origin
			return
		}
	}
}
