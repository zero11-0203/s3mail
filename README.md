# s3mail

**S**3 **S**ES **S**erverless mail: a personal, single-owner email system on AWS with a Go [JMAP](https://jmap.io/) backend.

## What it is

s3mail lets one owner receive and send mail for a custom domain without running a mail server. It supports many addresses with one account and one unified inbox. AWS managed services handle transport, storage and compute; a Go service speaks the standard JMAP protocol so existing clients can connect.

- **Receive:** Amazon SES accepts mail for declared aliases and stores the original message in S3.
- **Store:** originals are kept permanently in a versioned S3 bucket, and DynamoDB holds mailbox metadata and sync state.
- **Serve:** Go Lambdas behind API Gateway implement JMAP Core, Mail and Submission, including push and blob streaming.
- **Send:** a pluggable SMTP relay (SMTP2GO first) delivers outbound mail, with quotas and bounce handling. SES is used for receiving only.
- **Search:** SQLite FTS5 index snapshots in S3, rebuildable from the source data.
- **Sign in:** Cognito OAuth (password sign-in; MFA disabled by owner decision).
- **Tested Clients:** [aerc](https://aerc-mail.org/) and small CLI helpers first; browser webmail comes later.

![AWS architecture of s3mail](docs/diagrams/aws-mail-system.drawio.svg)

## Design goals

- Standard JMAP behavior that works with real clients, with no client-specific shortcuts.
- Serverless, low-cost operation for personal volume, with all infrastructure in Terraform.
- Least-privilege IAM for every role, and CI that cannot read mail.
- Durable, recoverable processing across asynchronous steps; a send with unknown relay acceptance is never retried automatically.
- Original messages are never deleted, even when an email is removed from the mailbox.

## Tech stack

Go · AWS (SES, S3, DynamoDB, Lambda, API Gateway, SQS/SNS, Cognito, Route 53, CloudWatch) · Terraform · GitHub Actions with OIDC · SQLite FTS5

## Documentation

- [Architecture](docs/architecture.md): five-minute system overview
- [Documentation index](docs/README.md): topic specifications and operations guides
- [AGENTS.md](AGENTS.md): guidance for coding agents working in this repository

## License

[MIT](LICENSE)
