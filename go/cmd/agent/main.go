// Command agent is the owner's CLI for the mail agent (docs/specs/agent.md).
//
//	agent login          consent once, so the token vault holds your tokens
//	agent ask QUESTION   ask about your mail; prints the cited answer
//	agent ask -thread ID QUESTION
//	                     follow up in an earlier conversation
//	agent digest [-peek] [-mail]
//	                     what arrived since the last digest; -peek keeps the
//	                     cursor, -mail delivers it to the Agent mailbox (A2)
//	agent draft [-reply-all] EMAIL_ID [INSTRUCTIONS]
//	                     save a reply draft in Drafts (A2)
//	agent remember TEXT | preferences | forget [ID...]
//	                     your stated preferences for drafts (A2)
//	agent act [-thread ID] REQUEST
//	                     propose flagging, moving or sending; nothing runs
//	                     until you approve (A3)
//	agent approve ID | reject ID
//	                     decide a proposal printed by act
//
// It calls the AgentCore runtime with your own AWS credentials and never
// sees a JMAP token. The runtime comes from -runtime-arn or
// $S3MAIL_AGENT_RUNTIME_ARN (terraform -chdir=infra/agent output -raw runtime_arn).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"

	"github.com/zero11-0203/s3mail/go/internal/agentcli"
	"github.com/zero11-0203/s3mail/go/internal/buildinfo"
)

const (
	loginTimeout = 5 * time.Minute
	// A question can take several model and tool steps.
	askTimeout = 3 * time.Minute
)

const usage = "usage: agent [-runtime-arn ARN] [-qualifier live|DEFAULT] login [-no-browser] [-force] | ask [-thread ID] QUESTION | digest [-peek] [-mail] | draft [-reply-all] EMAIL_ID [INSTRUCTIONS] | remember TEXT | preferences | forget [ID...] | act [-thread ID] REQUEST | approve ID | reject ID | version"

func main() {
	err := run()
	switch {
	case err == nil:
	case errors.Is(err, agentcli.ErrNotAuthorized):
		fmt.Fprintln(os.Stderr, "agent:", err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "agent:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	runtimeARN := fs.String("runtime-arn", os.Getenv("S3MAIL_AGENT_RUNTIME_ARN"), "AgentCore runtime ARN (default $S3MAIL_AGENT_RUNTIME_ARN)")
	qualifier := fs.String("qualifier", "live", "runtime endpoint: live, or DEFAULT for the newest candidate version")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return errors.New("a command is required")
	}
	if fs.Arg(0) == "version" {
		fmt.Println(buildinfo.String())
		return nil
	}
	if *runtimeARN == "" {
		return errors.New("set -runtime-arn or S3MAIL_AGENT_RUNTIME_ARN")
	}
	parsed, err := arn.Parse(*runtimeARN)
	if err != nil {
		return fmt.Errorf("runtime ARN: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(parsed.Region))
	if err != nil {
		return err
	}
	c := &agentcli.Client{API: bedrockagentcore.NewFromConfig(cfg), RuntimeARN: *runtimeARN, Qualifier: *qualifier}
	args := fs.Args()[1:]

	switch fs.Arg(0) {
	case "login":
		return login(ctx, c, args)
	case "ask":
		return ask(ctx, c, args)
	case "digest":
		return digest(ctx, c, args)
	case "draft":
		return draft(ctx, c, args)
	case "remember", "preferences", "forget":
		return preferences(ctx, c, fs.Arg(0), args)
	case "act":
		return act(ctx, c, args)
	case "approve", "reject":
		return decide(ctx, c, fs.Arg(0) == "approve", args)
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", fs.Arg(0))
	}
}

func login(ctx context.Context, c *agentcli.Client, args []string) error {
	fs := flag.NewFlagSet("agent login", flag.ContinueOnError)
	noBrowser := fs.Bool("no-browser", false, "print the consent URL without opening a browser")
	force := fs.Bool("force", false, "consent again even if authorized, such as after a scope was granted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	needed, err := c.Login(ctx, func(u string) {
		fmt.Printf("Open this URL to sign in:\n%s\n", agentcli.PlainText(u))
		if !*noBrowser {
			openBrowser(u)
		}
	}, *force)
	if err != nil {
		return err
	}
	if needed {
		fmt.Println("Authorized.")
	} else {
		fmt.Println("Already authorized.")
	}
	return nil
}

func ask(ctx context.Context, c *agentcli.Client, args []string) error {
	fs := flag.NewFlagSet("agent ask", flag.ContinueOnError)
	thread := fs.String("thread", "", "continue an earlier conversation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return errors.New("usage: agent ask [-thread ID] QUESTION")
	}
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	res, id, err := c.Ask(ctx, question, *thread)
	if err != nil {
		return err
	}
	fmt.Println(agentcli.PlainText(res.Answer))
	if res.NoMail() {
		fmt.Fprintln(os.Stderr, "\nagent: this answer did not consult your mail.")
	}
	fmt.Fprintf(os.Stderr, "\nthread: %s (follow up with -thread)\n", id)
	return nil
}

func digest(ctx context.Context, c *agentcli.Client, args []string) error {
	fs := flag.NewFlagSet("agent digest", flag.ContinueOnError)
	peek := fs.Bool("peek", false, "show the digest without moving the cursor")
	mail := fs.Bool("mail", false, "deliver the digest as an email in the Agent mailbox")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	deliver := "return"
	if *mail {
		deliver = "mail"
	}
	res, _, err := c.Call(ctx, map[string]any{"mode": "digest", "deliver": deliver, "peek": *peek}, "")
	if err != nil {
		return err
	}
	if *mail {
		fmt.Printf("Digest delivered to the Agent mailbox (email %s).\n", agentcli.PlainText(res.EmailID))
		return nil
	}
	fmt.Println(agentcli.PlainText(res.Digest))
	return nil
}

func draft(ctx context.Context, c *agentcli.Client, args []string) error {
	fs := flag.NewFlagSet("agent draft", flag.ContinueOnError)
	replyAll := fs.Bool("reply-all", false, "copy the other recipients")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: agent draft [-reply-all] EMAIL_ID [INSTRUCTIONS]")
	}
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	payload := map[string]any{
		"mode":         "draft",
		"email_id":     fs.Arg(0),
		"instructions": strings.TrimSpace(strings.Join(fs.Args()[1:], " ")),
		"reply_all":    *replyAll,
	}
	res, _, err := c.Call(ctx, payload, "")
	if err != nil {
		return err
	}
	if res.Draft == nil {
		return errors.New("agent error: no draft returned")
	}
	d := res.Draft
	fmt.Printf("Saved draft %s in Drafts:\nFrom: %s\nTo: %s\n", agentcli.PlainText(res.DraftID), agentcli.PlainText(d.From), agentcli.PlainText(strings.Join(d.To, ", ")))
	if len(d.Cc) > 0 {
		fmt.Printf("Cc: %s\n", agentcli.PlainText(strings.Join(d.Cc, ", ")))
	}
	fmt.Printf("Subject: %s\n\n%s\n", agentcli.PlainText(d.Subject), agentcli.PlainText(d.Body))
	fmt.Fprintln(os.Stderr, "\nagent: review and send it from aerc, or with agent act \"send draft "+agentcli.PlainText(res.DraftID)+"\".")
	return nil
}

func preferences(ctx context.Context, c *agentcli.Client, command string, args []string) error {
	payload := map[string]any{"mode": command}
	switch command {
	case "remember":
		text := strings.TrimSpace(strings.Join(args, " "))
		if text == "" {
			return errors.New("usage: agent remember TEXT")
		}
		payload["text"] = text
	case "forget":
		// No ids forgets every preference.
		if len(args) > 0 {
			payload["ids"] = args
		} else {
			payload["ids"] = nil
		}
	}
	res, _, err := c.Call(ctx, payload, "")
	if err != nil {
		return err
	}
	switch command {
	case "remember":
		if res.Preference != nil {
			fmt.Printf("Remembered (%s): %s\n", agentcli.PlainText(res.Preference.ID), agentcli.PlainText(res.Preference.Text))
		}
	case "forget":
		fmt.Printf("Forgot %d preference(s).\n", res.Forgotten)
	default:
		if len(res.Preferences) == 0 {
			fmt.Println("No preferences.")
		}
		for _, p := range res.Preferences {
			fmt.Printf("%s  %s\n", agentcli.PlainText(p.ID), agentcli.PlainText(p.Text))
		}
	}
	return nil
}

func act(ctx context.Context, c *agentcli.Client, args []string) error {
	fs := flag.NewFlagSet("agent act", flag.ContinueOnError)
	thread := fs.String("thread", "", "continue an earlier conversation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	request := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if request == "" {
		return errors.New("usage: agent act [-thread ID] REQUEST")
	}
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	res, id, err := c.Call(ctx, map[string]any{"mode": "act", "question": request}, *thread)
	if err != nil {
		return err
	}
	if res.Approval == nil {
		fmt.Println(agentcli.PlainText(res.Answer))
		fmt.Fprintf(os.Stderr, "\nagent: nothing proposed.\nthread: %s\n", id)
		return nil
	}
	a := res.Approval
	fmt.Println(agentcli.PlainText(a.Summary))
	fmt.Printf("\nNothing has run. Approve within 24 hours (until %s):\n  agent approve %s\nor reject:\n  agent reject %s\n",
		agentcli.PlainText(a.ExpiresAt), agentcli.ApprovalID(id, a.Hash), agentcli.ApprovalID(id, a.Hash))
	return nil
}

func decide(ctx context.Context, c *agentcli.Client, approve bool, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agent approve ID | agent reject ID")
	}
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	res, err := c.Decide(ctx, args[0], approve)
	if err != nil {
		return err
	}
	if res.Outcome == nil {
		return errors.New("agent error: no outcome returned")
	}
	o := res.Outcome
	switch o.Outcome {
	case "done":
		fmt.Printf("Done: %d email(s) changed.\n", len(o.Updated))
	case "partial":
		fmt.Printf("Partly done: %d changed, %d not changed.\n", len(o.Updated), len(o.Failed))
	case "sent":
		fmt.Printf("Sent (submission %s).\n", agentcli.PlainText(o.SubmissionID))
	case "rejected":
		fmt.Println("Rejected; nothing ran.")
	case "expired":
		fmt.Println("The approval expired; nothing ran. Propose again with agent act.")
	case "approval_mismatch":
		fmt.Println("That approval does not match the pending proposal; nothing ran.")
	case "draft_changed":
		fmt.Println("The draft changed after the proposal; nothing was sent. Propose again.")
	case "already_attempted":
		fmt.Printf("This approval was already used; not sent again. Submissions for the draft: %s\n", agentcli.PlainText(strings.Join(o.Submissions, ", ")))
	case "unknown":
		fmt.Printf("The send result is unknown and will not be retried. Check Sent in aerc. Submissions: %s\n", agentcli.PlainText(strings.Join(o.Submissions, ", ")))
	default:
		fmt.Printf("Failed: %s\n", agentcli.PlainText(orNone(o.Error, o.Outcome)))
	}
	return nil
}

func orNone(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "unknown"
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}
