package smtp2go

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPI(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Smtp2go-Api-Key") != "api-test" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"data":{"error":"bad key","error_code":"E_ApiResponseCodes.API_EXCEPTION"}}`))
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/v3/suppression/view":
			if in["continue_token"] == nil {
				_, _ = w.Write([]byte(`{"data":{"results":[{"email_address":"a@example.net","reason":"hardbounce","block_description":"550 unknown","timestamp":"2026-10-17T00:00:00Z"}],"continue_token":"next"}}`))
			} else {
				_, _ = w.Write([]byte(`{"data":{"results":[{"email_address":"b@example.net","reason":"spam"}],"continue_token":null}}`))
			}
		case "/v3/suppression/remove":
			if in["email_address"] != "a@example.net" || len(in["reasons"].([]any)) != 1 {
				t.Errorf("remove %v", in)
			}
			_, _ = w.Write([]byte(`{"data":{"suppressions":[{"email_address":"a@example.net","reason":"hardbounce","removed":true}]}}`))
		case "/v3/stats/email_cycle":
			_, _ = w.Write([]byte(`{"data":{"cycle_start":"2026-10-01","cycle_end":"2026-11-01","cycle_used":3,"cycle_remaining":997,"cycle_max":1000}}`))
		}
	}))
	defer srv.Close()
	a := &API{Key: "api-test", BaseURL: srv.URL + "/v3"}
	ctx := context.Background()

	sup, err := a.Suppressions(ctx)
	if err != nil || len(sup) != 2 || sup[0].Reason != "hardbounce" || sup[1].Address != "b@example.net" {
		t.Fatalf("suppressions %+v %v", sup, err)
	}
	rm, err := a.Unsuppress(ctx, "a@example.net", []string{"hardbounce"})
	if err != nil || len(rm) != 1 || !rm[0].Removed {
		t.Fatalf("remove %+v %v", rm, err)
	}
	c, err := a.EmailCycle(ctx)
	if err != nil || c.Max != 1000 || c.Used != 3 {
		t.Fatalf("cycle %+v %v", c, err)
	}
	if len(calls) != 4 {
		t.Errorf("calls %v", calls)
	}
	a.Key = "wrong"
	if _, err := a.EmailCycle(ctx); err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("error %v", err)
	}
}
