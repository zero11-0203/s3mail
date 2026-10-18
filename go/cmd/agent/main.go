// Command agent is the owner's CLI for the mail agent (docs/specs/agent.md).
//
//	agent login          consent once, so the token vault holds your tokens
//	agent ask QUESTION   ask about your mail; prints the cited answer
//	agent ask -thread ID QUESTION
//	                     follow up in an earlier conversation
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
		_, _ = fmt.Fprintln(fs.Output(), "usage: agent [-runtime-arn ARN] [-qualifier live|DEFAULT] login [-no-browser] | ask [-thread ID] QUESTION | version")
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

	switch fs.Arg(0) {
	case "login":
		return login(ctx, c, fs.Args()[1:])
	case "ask":
		return ask(ctx, c, fs.Args()[1:])
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", fs.Arg(0))
	}
}

func login(ctx context.Context, c *agentcli.Client, args []string) error {
	fs := flag.NewFlagSet("agent login", flag.ContinueOnError)
	noBrowser := fs.Bool("no-browser", false, "print the consent URL without opening a browser")
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
	})
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
