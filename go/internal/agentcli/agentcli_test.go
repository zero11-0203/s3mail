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

// consentReply is the runtime's login reply pointing at returnURL.
func consentReply(returnURL string) string {
	return `{"status":"authorization_required","authorization_url":"https://auth.example.com/consent","return_url":"` + returnURL + `"}`
}

// loginState is the state the CLI sent in its first invocation.
func loginState(t *testing.T, f *fakeAPI) string {
	t.Helper()
	var p map[string]string
	if err := json.Unmarshal(f.invokes[0].Payload, &p); err != nil || p["mode"] != "login" || len(p["state"]) < 43 {
		t.Fatalf("login payload %s", f.invokes[0].Payload)
	}
	return p["state"]
}

func TestLoginCompletesSessionBindingForTheOwner(t *testing.T) {
	returnURL := freeReturnURL(t)
	f := &fakeAPI{replies: []string{consentReply(returnURL), `{"status":"ok"}`}}
	const session = "urn:ietf:params:oauth:request_uri:ZTRlMTIw"
	opened := ""
	open := func(u string) {
		opened = u
		state := loginState(t, f)
		// The browser after consent: stray and forged requests first, then
		// the vault redirect.
		go func() {
			base := strings.TrimSuffix(returnURL, "/callback")
			if code := get(t, base+"/other?session_id="+session+"&state="+state); code != http.StatusNotFound {
				t.Errorf("other path: %d", code)
			}
			for _, q := range []string{
				"",
				"?session_id=" + session,
				"?session_id=" + session + "&state=forged",
				"?session_id=urn:session:1&state=" + state,
			} {
				if code := get(t, returnURL+q); code != http.StatusBadRequest {
					t.Errorf("%q: %d", q, code)
				}
			}
			resp, err := http.Post(returnURL+"?session_id="+session+"&state="+state, "text/plain", nil)
			if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("POST accepted: %v %v", resp, err)
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			get(t, returnURL+"?session_id="+session+"&state="+state)
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	needed, err := client(f).Login(ctx, open, false)
	if err != nil || !needed || opened != "https://auth.example.com/consent" {
		t.Fatalf("needed %v opened %q err %v", needed, opened, err)
	}
	if len(f.completed) != 1 || aws.ToString(f.completed[0].SessionUri) != session {
		t.Fatalf("completed %+v", f.completed)
	}
	if id, ok := f.completed[0].UserIdentifier.(*types.UserIdentifierMemberUserId); !ok || id.Value != OwnerUserID {
		t.Fatalf("user identifier %+v", f.completed[0].UserIdentifier)
	}
}

func TestLoginWhenAlreadyAuthorized(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"status":"ok"}`}}
	needed, err := client(f).Login(context.Background(), func(string) { t.Error("opened a browser") }, false)
	if err != nil || needed || len(f.completed) != 0 {
		t.Fatalf("needed %v err %v", needed, err)
	}
	loginState(t, f)
}

func TestForcedLoginAsksForConsentAgain(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"status":"ok"}`}}
	if _, err := client(f).Login(context.Background(), func(string) {}, true); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(f.invokes[0].Payload, &payload); err != nil || payload["force"] != true {
		t.Fatalf("payload %v err %v", payload, err)
	}
}

func TestLoginStatesAreFresh(t *testing.T) {
	if a, b := NewLoginState(), NewLoginState(); a == b || len(a) != 43 {
		t.Fatalf("states %q %q", a, b)
	}
}

func TestLoginTimesOutWithoutRedirect(t *testing.T) {
	f := &fakeAPI{replies: []string{consentReply(freeReturnURL(t))}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := client(f).Login(ctx, func(string) {}, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	if len(f.completed) != 0 {
		t.Fatal("completed without a redirect")
	}
}

func TestLoginRefusesANonLoopbackReturnURL(t *testing.T) {
	f := &fakeAPI{replies: []string{consentReply("https://evil.example.com/callback")}}
	if _, err := client(f).Login(context.Background(), func(string) { t.Error("opened a browser") }, false); err == nil {
		t.Fatal("accepted a remote return URL")
	}
}

func TestPlainTextDropsTerminalControls(t *testing.T) {
	in := "Total \x1b[31m42\x1b[0m EUR\x1b]52;c;cGF3bmVk\x07\r\nnext\tline \u202edesrever\u202c \u0085 \xff 日本語"
	want := "Total [31m42[0m EUR]52;c;cGF3bmVk\nnext\tline desrever  \ufffd 日本語"
	if got := PlainText(in); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestListenServesBothLoopbacksBesideAnIPv6Squatter(t *testing.T) {
	returnURL := freeReturnURL(t)
	port := strings.TrimSuffix(strings.TrimPrefix(returnURL, "http://localhost:"), "/callback")
	// Another program on [::]:<port>, as a local database might be.
	squatter, err := net.Listen("tcp6", "[::]:"+port)
	if err != nil {
		t.Skipf("no IPv6: %v", err)
	}
	defer func() { _ = squatter.Close() }()

	ln, _, err := Listen(returnURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	}()
	// The squatter never answers, so a request that reaches it times out.
	c := http.Client{Timeout: 2 * time.Second}
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		resp, err := c.Get("http://" + host + ":" + port + "/callback") //nolint:noctx // test request to the local listener
		if err != nil {
			t.Errorf("%s: %v", host, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusTeapot {
			t.Errorf("%s: %d", host, resp.StatusCode)
		}
	}
}

func TestReturnURLMustBeLoopbackHTTP(t *testing.T) {
	for _, u := range []string{"https://localhost:8765/callback", "http://example.com:8765/callback", "http://localhost/callback"} {
		if _, _, err := Listen(u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}
