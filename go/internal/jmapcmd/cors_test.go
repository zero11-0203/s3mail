package jmapcmd

import (
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestAllowOrigin(t *testing.T) {
	const web = "https://webmail.example.com"
	for _, tc := range []struct {
		name       string
		origin     string
		reqHeaders map[string]string
		vary       string
		wantAllow  string
		wantVary   string
	}{
		{name: "disabled", origin: "", reqHeaders: map[string]string{"origin": web}},
		{name: "registered origin", origin: web, reqHeaders: map[string]string{"origin": web}, wantAllow: web, wantVary: "Origin"},
		{name: "header case", origin: web, reqHeaders: map[string]string{"Origin": web}, wantAllow: web, wantVary: "Origin"},
		{name: "other origin", origin: web, reqHeaders: map[string]string{"origin": "https://evil.example.com"}, wantVary: "Origin"},
		{name: "no origin", origin: web, wantVary: "Origin"},
		{name: "existing vary", origin: web, reqHeaders: map[string]string{"origin": web}, vary: "Accept-Encoding", wantAllow: web, wantVary: "Accept-Encoding, Origin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &events.APIGatewayProxyStreamingResponse{StatusCode: 200}
			if tc.vary != "" {
				resp.Headers = map[string]string{"Vary": tc.vary}
			}
			allowOrigin(tc.origin, events.APIGatewayProxyRequest{Headers: tc.reqHeaders}, resp)
			if got := resp.Headers["Access-Control-Allow-Origin"]; got != tc.wantAllow {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tc.wantAllow)
			}
			if got := resp.Headers["Vary"]; got != tc.wantVary {
				t.Errorf("Vary = %q, want %q", got, tc.wantVary)
			}
		})
	}
	allowOrigin(web, events.APIGatewayProxyRequest{}, nil) // a nil response is left alone
}
