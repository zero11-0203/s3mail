# Security policy

s3mail stores and sends personal email, so security reports are taken seriously.

## Reporting a vulnerability

Report vulnerabilities privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**. Only the maintainers can read the report. Do not open a public issue or pull request for a vulnerability.

Include:

- the affected component (for example the JMAP API, authorizer, ingestion, relay webhook, `mail-auth`, Terraform module or the agent);
- steps to reproduce, using synthetic mail and `example.com` addresses only;
- the impact you expect.

Never include real mail, access tokens, AWS account IDs or other secrets in a report. Redact them from logs.

You can expect an acknowledgement within a week. Fixes land on `main`; there are no maintained release branches yet.

## Supported versions

s3mail is a preview without tagged releases. Only the latest commit on `main` is supported. Deployments update by pulling `main`, rebuilding and applying Terraform ([deploy guide](docs/guides/deploy.md#updating)).

## Security model

The [security spec](docs/specs/security.md) owns the design: OAuth bearer tokens on every JMAP route, per-method scopes, least-privilege IAM for every role and no AWS credentials in CI. Know its limits before you rely on it:

- **AWS account administrators can read all mail.** Protect the account with MFA and short-lived credentials, and do not deploy into an account that others administer.
- **Sign-in is password-only.** MFA is off in the current configuration (an [accepted risk](docs/specs/security.md#authentication-and-authorization)); a phished or reused password gives full read and send access. Use a long, unique, generated password.
- **Originals cannot be erased.** Received and sent messages are kept permanently in a versioned bucket that denies deletes, so mail cannot be removed on request. Consider this before receiving mail that data-protection rules may require you to delete.
- **Spam and virus verdicts are signals,** not filtering guarantees. Mail failing the sender-authentication policy goes to Quarantine.
- **Optional features send mail text outside AWS.** Semantic search and the mail agent call an external model API (OpenRouter); see [data leaving AWS](docs/specs/security.md#data-leaving-aws). Both are off by default.

## In scope

- Authentication or authorization bypass on any JMAP route, upload, download or EventSource stream.
- Access to another account's or another client's data, or to scopes a token does not hold.
- Mail content reaching logs, metrics, traces or error responses.
- IAM grants broader than the [permission inventory](docs/operations/permissions.md) documents, including any path that deletes originals.
- MIME parsing that crashes, hangs or exceeds its resource bounds on crafted input.
- A submission being sent twice, or sent without the owner's request.
- Prompt injection through mail that makes the agent act outside its approval rules.

Issues in AWS services, SMTP2GO, aerc or other upstream projects should be reported to those projects.
