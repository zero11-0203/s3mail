# s3mail

**S**3 **S**ES **S**erverless mail: a self-hosted, single-owner email system on AWS with a Go [JMAP](https://jmap.io/) backend and an optional AI mail agent.

> **Preview.** s3mail runs on a reference deployment with aerc, but it has not yet passed all of its acceptance checks. Try it on a spare domain or a subdomain before you move your everyday mail. See [limitations](#limitations).

## What it is

s3mail lets one owner receive and send mail for a custom domain without running a mail server. It supports many addresses with one account and one unified inbox. AWS managed services handle transport, storage and compute; a Go service speaks the standard JMAP protocol so existing clients can connect.

- **Receive:** Amazon SES accepts mail for declared aliases and stores the original message in S3.
- **Store:** originals are kept permanently in a versioned S3 bucket, and DynamoDB holds mailbox metadata and sync state.
- **Serve:** Go Lambdas behind API Gateway implement JMAP Core, Mail and Submission, including push and blob streaming.
- **Send:** a pluggable SMTP relay (SMTP2GO first) delivers outbound mail, with quotas and bounce handling. SES is used for receiving only.
- **Search:** SQLite FTS5 index snapshots in S3, rebuildable from the source data, with optional semantic search.
- **Sign in:** Cognito OAuth with a terminal helper, `mail-auth`, that hands short-lived tokens to your mail client.
- **Clients:** [aerc](https://aerc-mail.org/) is the tested client. A browser webmail is an optional, separately deployed part.

### Mail agent

s3mail includes an optional AI assistant for your mailbox, built as a [LangGraph](https://langchain-ai.github.io/langgraph/) graph on Amazon Bedrock AgentCore:

- **Ask:** answers questions about your mail from the command line, citing the emails it read.
- **Triage:** sorts each new email by category and priority and flags what needs a reply, then sums it up in a daily digest.
- **Draft:** writes reply drafts into your Drafts mailbox for you to edit and send.
- **Act with approval:** proposes moves, archiving or sending a reviewed draft, and runs each one only after you approve it.

```sh
agent ask "What did the billing emails this month say?"
agent draft <email id> "Accept, and suggest Thursday instead."
agent act "Archive the newsletters from last week."   # prints an approval id
agent approve <approval id>
```

The agent is just another JMAP client. It has its own OAuth client and scopes and no AWS access to mail storage, and each new kind of side effect is enabled only after its evaluation gate passes. Mail stays usable without it. When enabled, it sends mail excerpts to an external model API (OpenRouter). See the [agent spec](docs/specs/agent.md) and [`agent/`](agent/README.md).

![AWS architecture of s3mail](docs/diagrams/aws-mail-system.drawio.svg)

## Limitations

- **JMAP only:** no IMAP and no SMTP submission for other clients, so you need a JMAP client.
- **One user per deployment,** and one deployment per AWS account and region: resource names are fixed.
- **AWS-specific setup:** DNS must be hosted in a new Route 53 zone that s3mail creates, and s3mail activates its own SES receipt rule set, which replaces any active one in that region.
- **SMTP2GO** is the only relay configured today.
- **macOS only for `mail-auth`:** the sign-in helper stores its session in the macOS Keychain.
- **Browser webmail is not validated:** its Terraform and image build exist, but it has not been deployed.
- **Originals can never be deleted,** by design; deleting mail in a client only hides it.
- **No backups:** DynamoDB holds mailbox metadata without point-in-time recovery. Losing it loses flags and folders; the originals remain.
- **Password-only sign-in:** MFA is off in the current configuration, an accepted risk described in the [security spec](docs/specs/security.md#authentication-and-authorization).
- **No alarms by default;** problems show on the CloudWatch dashboard.

## Get started

1. Read [costs](docs/guides/costs.md): about $2–4 a month for personal volume, plus a domain and the relay.
2. Follow the [deploy guide](docs/guides/deploy.md) to deploy into your own AWS account.
3. Use the [usage guide](docs/guides/usage.md) to sign in, set up aerc and handle sending limits.

## Develop

You need Go (see `go/go.mod`), Terraform 1.16.6, [golangci-lint](https://golangci-lint.run/), [gitleaks](https://github.com/gitleaks/gitleaks) and [uv](https://docs.astral.sh/uv/). The checks run offline, without an AWS account:

```sh
make check    # gofmt, go vet, golangci-lint, go test -race, govulncheck, the agent tests,
              # terraform fmt/validate/test and gitleaks
make build    # linux/arm64 Lambda zips in go/build/
```

| Path | Contents |
| --- | --- |
| `go/` | Lambda handlers and the `mail-auth`, `mail-admin` and `agent` CLIs |
| `infra/` | Terraform roots (`bootstrap`, `main`, `webmail`, `agent`) and modules with `terraform test` cases |
| `agent/` | The optional Python mail agent, its tests and evaluation sets |
| `webmail/` | Version pins and Dockerfile for the optional browser webmail |
| `docs/` | Architecture, guides, topic specifications and operations docs |

## Design goals

- Standard JMAP behavior that works with real clients, with no client-specific shortcuts.
- Serverless, low-cost operation for personal volume, with all infrastructure in Terraform.
- Least-privilege IAM for every role, and CI that cannot read mail.
- Durable, recoverable processing across asynchronous steps; a send with unknown relay acceptance is never retried automatically.
- Original messages are never deleted, even when an email is removed from the mailbox.

## Documentation

- [Architecture](docs/architecture.md): five-minute system overview
- [Documentation index](docs/README.md): guides, topic specifications and operations docs
- [AGENTS.md](AGENTS.md): guidance for coding agents working in this repository

## Contributing and security

Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md), not in public issues. This project follows a [code of conduct](CODE_OF_CONDUCT.md).

## License

[MIT](LICENSE)
