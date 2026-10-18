package agentcli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
)

const testARN = "arn:aws:bedrock-agentcore:us-east-1:111111111111:runtime/s3mail_agent-abc"

type fakeAPI struct {
	replies   []string
	invokes   []*bedrockagentcore.InvokeAgentRuntimeInput
	completed []*bedrockagentcore.CompleteResourceTokenAuthInput
}

func (f *fakeAPI) InvokeAgentRuntime(_ context.Context, in *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
	f.invokes = append(f.invokes, in)
	if len(f.replies) == 0 {
		return nil, errors.New("no scripted reply")
	}
	reply := f.replies[0]
	f.replies = f.replies[1:]
	return &bedrockagentcore.InvokeAgentRuntimeOutput{Response: io.NopCloser(strings.NewReader(reply))}, nil
}

func (f *fakeAPI) CompleteResourceTokenAuth(_ context.Context, in *bedrockagentcore.CompleteResourceTokenAuthInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.CompleteResourceTokenAuthOutput, error) {
	f.completed = append(f.completed, in)
	return &bedrockagentcore.CompleteResourceTokenAuthOutput{}, nil
}

func client(f *fakeAPI) *Client {
	return &Client{API: f, RuntimeARN: testARN, Qualifier: "live"}
}

func TestSessionIDsAreLongEnoughAndUnique(t *testing.T) {
	a, b := NewSessionID(), NewSessionID()
	if len(a) < 33 || a == b || !strings.HasPrefix(a, "cli-") {
		t.Fatalf("session ids %q, %q", a, b)
	}
}

func TestAskSendsOwnerQuestionAndLiveQualifier(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"status":"ok","answer":"7pm [email:M1]","citations":["M1"]}`}}
	res, thread, err := client(f).Ask(context.Background(), "When is dinner?", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "7pm [email:M1]" || len(res.Citations) != 1 {
		t.Fatalf("result %+v", res)
	}
	in := f.invokes[0]
	if aws.ToString(in.RuntimeUserId) != OwnerUserID || aws.ToString(in.Qualifier) != "live" || aws.ToString(in.AgentRuntimeArn) != testARN {
		t.Fatalf("invoke %+v", in)
	}
	if aws.ToString(in.RuntimeSessionId) != thread || len(thread) < 33 {
		t.Fatalf("thread %q", thread)
	}
	var payload map[string]string
	if err := json.Unmarshal(in.Payload, &payload); err != nil || payload["mode"] != "ask" || payload["question"] != "When is dinner?" {
		t.Fatalf("payload %s", in.Payload)
	}
}

func TestAskContinuesAThread(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"status":"ok","answer":"x"}`}}
	_, thread, err := client(f).Ask(context.Background(), "and then?", "cli-earlier-thread-000000000000000000")
	if err != nil || thread != "cli-earlier-thread-000000000000000000" || aws.ToString(f.invokes[0].RuntimeSessionId) != thread {
		t.Fatalf("thread %q err %v", thread, err)
	}
}

func TestAskReportsMissingAuthorizationAndErrors(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"status":"authorization_required"}`, `{"status":"error","error":"budget_exceeded"}`}}
	if _, _, err := client(f).Ask(context.Background(), "q", ""); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("want ErrNotAuthorized, got %v", err)
	}
	if _, _, err := client(f).Ask(context.Background(), "q", ""); err == nil || !strings.Contains(err.Error(), "budget_exceeded") {
		t.Fatalf("want budget error, got %v", err)
	}
}

func freeReturnURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return "http://localhost:" + strconv.Itoa(port) + "/callback"
}

func get(t *testing.T, u string) int {
	t.Helper()
	resp, err := http.Get(u) //nolint:noctx // test request to the local listener
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestLoginCompletesSessionBindingForTheOwner(t *testing.T) {
	returnURL := freeReturnURL(t)
	f := &fakeAPI{replies: []string{
		`{"status":"authorization_required","authorization_url":"https://auth.example.com/consent"}`,
		`{"status":"ok"}`,
	}}
	opened := ""
	open := func(u string) {
		opened = u
		// The browser after consent: stray requests first, then the vault redirect.
		go func() {
			base := strings.TrimSuffix(returnURL, "/callback")
			if code := get(t, base+"/other?session_id=x"); code != http.StatusNotFound {
				t.Errorf("other path: %d", code)
			}
			if code := get(t, returnURL); code != http.StatusBadRequest {
				t.Errorf("no session id: %d", code)
			}
			get(t, returnURL+"?session_id=urn:session:1")
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	needed, err := client(f).Login(ctx, returnURL, open)
	if err != nil || !needed || opened != "https://auth.example.com/consent" {
		t.Fatalf("needed %v opened %q err %v", needed, opened, err)
	}
	if len(f.completed) != 1 || aws.ToString(f.completed[0].SessionUri) != "urn:session:1" {
		t.Fatalf("completed %+v", f.completed)
	}
	if id, ok := f.completed[0].UserIdentifier.(*types.UserIdentifierMemberUserId); !ok || id.Value != OwnerUserID {
		t.Fatalf("user identifier %+v", f.completed[0].UserIdentifier)
	}
}

func TestLoginWhenAlreadyAuthorized(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"status":"ok"}`}}
	needed, err := client(f).Login(context.Background(), freeReturnURL(t), func(string) { t.Error("opened a browser") })
	if err != nil || needed || len(f.completed) != 0 {
		t.Fatalf("needed %v err %v", needed, err)
	}
}

func TestLoginTimesOutWithoutRedirect(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"status":"authorization_required","authorization_url":"https://auth.example.com/c"}`}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := client(f).Login(ctx, freeReturnURL(t), func(string) {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	if len(f.completed) != 0 {
		t.Fatal("completed without a redirect")
	}
}

func TestReturnURLMustBeLoopbackHTTP(t *testing.T) {
	for _, u := range []string{"https://localhost:8765/callback", "http://example.com:8765/callback", "http://localhost/callback"} {
		if _, _, err := Listen(u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}
