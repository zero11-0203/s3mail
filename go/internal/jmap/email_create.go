package jmap

import (
	"context"
	"encoding/json"
)

// createEmail composes and archives a new Email (RFC 8621 §4.6).
func (s *Server) createEmail(ctx context.Context, c *call, raw json.RawMessage) (map[string]any, State, *SetError) {
	return nil, State{}, &SetError{Type: "forbidden", Description: "Email creation is not supported yet"}
}
