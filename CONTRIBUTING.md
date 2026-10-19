# Contributing to s3mail

Thanks for your interest. s3mail has one maintainer, so small, focused changes are the easiest to review.

## Before you start

- **Bugs and small fixes:** open a pull request directly, or an issue if you are unsure.
- **Larger changes** (new features, new relays or DNS providers, changes to storage or authorization): open an issue first, so the design can be agreed before you write code.
- **Security problems:** do not open an issue; follow [SECURITY.md](SECURITY.md).

Read the [architecture overview](docs/architecture.md) and the [topic spec](docs/README.md#topic-specifications) that owns the area you are changing. Each spec owns its requirements, failure behavior and acceptance checks.

## Development setup

You need:

- Go, at the version in `go/go.mod`
- Terraform 1.16.6 (pinned in `infra/*/versions.tf`)
- [golangci-lint](https://golangci-lint.run/) v2
- [gitleaks](https://github.com/gitleaks/gitleaks)
- [uv](https://docs.astral.sh/uv/) for the optional Python agent

Then run the same checks CI runs (`.github/workflows/checks.yml`). They need no AWS account or network access beyond downloading dependencies:

```sh
make check
```

Useful narrower targets: `make go-check`, `make agent-check`, `make tf-check` and `make secrets`.

Testing against AWS is optional and needs your own account; follow the [deploy guide](docs/guides/deploy.md). Use a spare domain or a subdomain.

## Rules every change must keep

These protect people's mail. The [repository guidance](AGENTS.md#implementation-constraints) lists them in full.

- **Synthetic data only.** Fixtures, examples, logs and issues use made-up mail and `example.com` addresses. Never commit real messages, addresses, account IDs, domains or secrets. `gitleaks` cannot detect personal mail, so reviewers check this by hand.
- **Original messages are never deleted.** No code path, IAM grant or lifecycle rule may delete objects or versions in the originals bucket.
- **No automatic resend** of a submission whose relay acceptance is unknown.
- **Least privilege** for every IAM role or policy you add; update the [permission inventory](docs/operations/permissions.md) in the same change.
- **No client-specific shortcuts** in the JMAP server; fix the client or document the incompatibility.
- **No mail content in telemetry:** logs, metrics and traces stay content-free.

## Making a change

1. Fork the repository and create a branch.
2. Make the change with tests. Go code is formatted with `gofmt`; Python with `ruff format`; Terraform with `terraform fmt`.
3. Update the owning spec, its failure behavior and its acceptance checks together when behavior changes. Update the guides in `docs/guides/` when variables, outputs or commands change.
4. If you change AWS components, integrations, data flows, authorization boundaries or hosting, update the [architecture diagram](docs/diagrams/README.md) in the same change, or explain in the pull request why it needs no update.
5. Run `make check`.
6. Open a pull request against `main` and fill in the template.

Pull requests from forks run the offline checks only and never receive AWS credentials. The maintainer reviews and lands changes on `main`.

### Commit messages

Use a short imperative summary line (for example, "Reject uploads over maxSizeUpload before reading the body"), then explain why the change is needed and any trade-offs.

## License

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE).
