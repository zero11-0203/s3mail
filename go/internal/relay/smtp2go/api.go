package smtp2go

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// API is the SMTP2GO REST API client the owner's tools use to inspect and
// clear the suppression list and read the plan's sending cycle. Its key
// needs only the suppression and statistics permissions.
type API struct {
	Key string
	// BaseURL defaults to https://api.smtp2go.com/v3.
	BaseURL string
	// HTTP defaults to a client with a 30 s timeout.
	HTTP *http.Client
}

// Suppression is one blocked address or domain.
type Suppression struct {
	Address     string `json:"email_address"`
	Reason      string `json:"reason"`
	Description string `json:"block_description"`
	Timestamp   string `json:"timestamp"`
}

// Removal is the result of clearing one reason for an address.
type Removal struct {
	Address string `json:"email_address"`
	Reason  string `json:"reason"`
	Removed bool   `json:"removed"`
}

// Cycle is the plan's current sending cycle.
type Cycle struct {
	Start     string `json:"cycle_start"`
	End       string `json:"cycle_end"`
	Used      int64  `json:"cycle_used"`
	Remaining int64  `json:"cycle_remaining"`
	Max       int64  `json:"cycle_max"`
}

func (a *API) call(ctx context.Context, path string, in, out any) error {
	base := a.BaseURL
	if base == "" {
		base = "https://api.smtp2go.com/v3"
	}
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Smtp2go-Api-Key", a.Key)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("smtp2go %s: HTTP %d: %w", path, resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error     string `json:"error"`
			ErrorCode string `json:"error_code"`
		}
		_ = json.Unmarshal(env.Data, &e)
		return fmt.Errorf("smtp2go %s: HTTP %d: %s %s", path, resp.StatusCode, e.ErrorCode, e.Error)
	}
	return json.Unmarshal(env.Data, out)
}

// Suppressions returns the whole suppression list.
func (a *API) Suppressions(ctx context.Context) ([]Suppression, error) {
	var all []Suppression
	in := map[string]any{}
	for {
		var page struct {
			Results  []Suppression `json:"results"`
			Continue *string       `json:"continue_token"`
		}
		if err := a.call(ctx, "/suppression/view", in, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if page.Continue == nil || *page.Continue == "" {
			return all, nil
		}
		in["continue_token"] = *page.Continue
	}
}

// Unsuppress clears the given reasons for an address or domain.
func (a *API) Unsuppress(ctx context.Context, address string, reasons []string) ([]Removal, error) {
	var out struct {
		Suppressions []Removal `json:"suppressions"`
	}
	err := a.call(ctx, "/suppression/remove", map[string]any{"email_address": address, "reasons": reasons}, &out)
	return out.Suppressions, err
}

// EmailCycle returns the plan's current cycle and its usage.
func (a *API) EmailCycle(ctx context.Context) (Cycle, error) {
	var c Cycle
	err := a.call(ctx, "/stats/email_cycle", map[string]any{}, &c)
	return c, err
}
