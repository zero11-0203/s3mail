# s3mail

**S**3 **S**ES **S**erverless mail: a self-hosted, single-owner email system on AWS with a Go [JMAP](https://jmap.io/) backend.

> **Preview.** s3mail runs on a reference deployment with aerc, but it has not yet passed all of its acceptance checks. Try it on a spare domain or a subdomain before you move your everyday mail. See [status](#status).

## What it is

s3mail lets one owner receive and send mail for a custom domain without running a mail server. It supports many addresses with one account and one unified inbox. AWS managed services handle transport, storage and compute; a Go service speaks the standard JMAP protocol so existing clients can connect.

- **Receive:** Amazon SES accepts mail for declared aliases and stores the original message in S3.
- **Store:** originals are kept permanently in a versioned S3 bucket, and DynamoDB holds mailbox metadata and sync state.
- **Serve:** Go Lambdas behind API Gateway implement JMAP Core, Mail and Submission, including push and blob streaming.
- **Send:** a pluggable SMTP relay (SMTP2GO first) delivers outbound mail, with quotas and bounce handling. SES is used for receiving only.
- **Search:** SQLite FTS5 index snapshots in S3, rebuildable from the source data, with optional semantic search.
- **Sign in:** Cognito OAuth with a terminal helper, `mail-auth`, that hands short-lived tokens to your mail client.
- **Clients:** [aerc](https://aerc-mail.org/) is the tested client. A browser webmail and an AI mail agent are optional, separately deployed parts.

![AWS architecture of s3mail](docs/diagrams/aws-mail-system.drawio.svg)

## Status

| Part | State |
| --- | --- |
| Core mail system: receiving, storage, JMAP, push, blobs, search and sending | Implemented with offline tests and running on a reference deployment with aerc. Many acceptance checks are so far covered by unit tests only; deployed failure-injection runs, the release canary and recovery rehearsals are not done. |
| `mail-auth` and `mail-admin` CLIs | Implemented. `mail-auth` stores its session in the macOS Keychain, so it runs on macOS only for now. |
| Semantic search | Implemented, off by default. Sends indexed mail text to OpenRouter when enabled. |
| Mail agent (`agent/`, `infra/agent/`) | Stages A0–A3 and the A4 tools implemented; no stage gate has passed in full. Optional. |
| Browser webmail (`webmail/`, `infra/webmail/`) | Terraform and image build exist; not deployed or validated. |
| Deployment pipeline | CI runs offline checks only. Deploying is a local, saved-plan `terraform apply`; the CI deploy roles and post-deploy canary are designed but not built. |

Each topic spec's **Acceptance checks** section is its release gate; see the [documentation index](docs/README.md).

## Limitations

- **JMAP only:** no IMAP and no SMTP submission for other clients, so you need a JMAP client.
- **One user per deployment,** and one deployment per AWS account and region: resource names are fixed.
- **AWS-specific setup:** DNS must be hosted in a new Route 53 zone that s3mail creates, and s3mail activates its own SES receipt rule set, which replaces any active one in that region.
- **SMTP2GO** is the only relay configured today.
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
