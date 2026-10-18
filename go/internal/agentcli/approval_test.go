package agentcli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func TestApprovalIDsRoundTrip(t *testing.T) {
	thread, hash := NewSessionID(), strings.Repeat("ab", 32)
	gotThread, gotHash, err := ParseApprovalID(ApprovalID(thread, hash))
	if err != nil || gotThread != thread || gotHash != hash {
		t.Fatalf("%q %q %v", gotThread, gotHash, err)
	}
	for _, bad := range []string{"", hash, "short:" + hash, thread + ":" + strings.Repeat("g", 64), thread + ":" + hash[:63]} {
		if _, _, err := ParseApprovalID(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDecideResumesTheProposalThread(t *testing.T) {
	thread, hash := NewSessionID(), strings.Repeat("0f", 32)
	f := &fakeAPI{replies: []string{`{"status":"ok","outcome":{"outcome":"sent","submission_id":"S1"}}`}}
	res, err := client(f).Decide(context.Background(), ApprovalID(thread, hash), true)
	if err != nil || res.Outcome == nil || res.Outcome.Outcome != "sent" || res.Outcome.SubmissionID != "S1" {
		t.Fatalf("res %+v err %v", res, err)
	}
	in := f.invokes[0]
	if aws.ToString(in.RuntimeSessionId) != thread {
		t.Fatalf("session %q", aws.ToString(in.RuntimeSessionId))
	}
	var payload map[string]any
	if err := json.Unmarshal(in.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["mode"] != "approve" || payload["hash"] != hash || payload["approve"] != true {
		t.Fatalf("payload %v", payload)
	}
}

func TestPendingApprovalsAreNotErrors(t *testing.T) {
	f := &fakeAPI{replies: []string{
		`{"status":"approval_required","approval":{"hash":"` + strings.Repeat("1", 64) + `","kind":"update","summary":"Change 1 email(s)","expires_at":"2026-10-19T00:00:00+00:00"}}`,
		`{"status":"error","error":"scope_not_granted"}`,
	}}
	res, _, err := client(f).Call(context.Background(), map[string]any{"mode": "act", "question": "archive"}, "")
	if err != nil || res.Approval == nil || res.Approval.Kind != "update" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if _, _, err := client(f).Call(context.Background(), map[string]any{"mode": "act"}, ""); err == nil || !strings.Contains(err.Error(), "scope_not_granted") {
		t.Fatalf("err %v", err)
	}
}
