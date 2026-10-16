# Phase 0 evidence: aerc read slice

[Documentation index](../README.md) · [Roadmap](../roadmap.md#phase-0--prove-the-risky-integrations)

**Status:** measured on 2026-10-17. This page records results, not requirements; they live in [JMAP](../specs/jmap.md), [security](../specs/security.md#shared-endpoint-authorization) and [clients](../specs/clients.md). Method coverage is in the [conformance matrix](../specs/jmap-conformance.md). Submission, the last part of the roadmap's aerc slice, is not built yet.

## Setup

- `infra/modules/jmap-api` in `us-east-1`: a Regional REST API, stage `v1` on the `execute-api` hostname (no custom domain yet), throttled to 5 requests/s with a burst of 10. Every JMAP route uses the bearer-token `TOKEN` authorizer with result caching off. Only `/.well-known/oauth-protected-resource` (static `MOCK`) is public.
- Routes, each a `STREAM` integration with a 900,000 ms timeout on the function's `live` alias:

  | Route | Function | Memory, timeout |
  | --- | --- | --- |
  | `GET /.well-known/jmap`, `POST /jmap` | `s3mail-jmap-api` | 1024 MB, 30 s |
  | `GET /download/{accountId}/{blobId}/{name}` | `s3mail-jmap-download` | 1024 MB, 60 s |
  | `GET /eventsource` | `s3mail-jmap-push` | 128 MB, 900 s, reserved concurrency 4 |

- Each function has its own role ([inventory](../operations/permissions.md)). The owner's Cognito `sub` is the only subject mapped to the account; it is set in the gitignored `terraform.tfvars`.
- Data: the two synthetic probe messages from the [SES receiving round](phase0-ses-receiving.md), one in Inbox and one in Quarantine.
- Client: aerc 0.22.0 (Homebrew, macOS), with `source = jmap+oauthbearer://<api host>/v1/.well-known/jmap` and `source-cred-cmd = mail-auth token`. On macOS, aerc reads `~/Library/Preferences/aerc/accounts.conf`, not `~/.config/aerc`.

## Results

| Check | Result |
| --- | --- |
| Any JMAP route without a token, or with a malformed bearer token | `401` with `WWW-Authenticate: Bearer resource_metadata="<base>/.well-known/oauth-protected-resource"`; the metadata names the Cognito issuer and the three scopes |
| Session with a `mail-auth` access token | Core and Mail capabilities with the advertised limits; `apiUrl`, `downloadUrl` and `eventSourceUrl` under the stage URL |
| `Mailbox/get` → `Email/query` → `Email/get` by result reference | Counts match the stored Emails; `bodyStructure` and `textBody` parsed from the raw original in S3 by exact version; part blob ids returned |
| `Email/changes` from `1-0` | Both receipts reported as created |
| `Email/query` with a `text` filter | `unsupportedFilter` (search is not built) |
| Download of the raw message blob | `200`, byte count equal to the stored size, `Content-Disposition: attachment` with an RFC 8187 filename, `Content-Security-Policy: sandbox; default-src 'none'`, `nosniff` |
| Download of an unknown blob id | `404` |
| EventSource with `closeafter=state` while `Email/set` set `$seen` | `: open`, then a `state` event at the current state, then one at the new state, then the stream closed. Reverting the keyword advanced the state again |
| aerc start | Connected; folder list shows Archive, Drafts, Inbox (1), Junk, Quarantine (1), Sent and Trash; the Inbox message is listed. Calls: `Mailbox/get`, `Email/query`, `Email/get`, `Thread/get`; one EventSource opened with `types` Email, Mailbox, Thread |
| aerc opens the message | Headers and body shown; one download of part `1`; aerc's automatic `$seen` `Email/set` succeeded, followed by its `Email/changes`, `Mailbox/changes` and `Thread/changes` calls from the push event |

## Findings

1. **Reserved words in projections.** The first deployment's `Email/query` failed with `ValidationException: Invalid ProjectionExpression`: `blob` is a DynamoDB reserved word, and the unit tests' fake table didn't check expressions. Every projected attribute is now aliased, and a unit test checks that the projection uses only aliases.
2. **aerc's default folder is matched by name.** `default = INBOX` didn't select the `Inbox` mailbox, so aerc opened the first folder (Archive). The account config uses `default = Inbox`; the server keeps the RFC role and the display name.
3. **Abandoned streams stay open until a write fails.** When aerc is killed, the push function only notices at the next keepalive (at most 60 s quiet), so each abandoned stream holds one of the four reserved push slots for up to a minute. Connection leases are still to do ([JMAP push](../specs/jmap.md#push)).
4. **Plans stay clean only with the default gateway-response template declared.** API Gateway adds a JSON body template to the `UNAUTHORIZED` response; the module now declares it.

## Not covered yet

- Submission (`Identity/*`, `EmailSubmission/*`) and upload. `uploadUrl` is advertised because the session object requires it, but nothing serves it.
- Threading by references, Mailbox mutation, `Email/import`/`destroy`, query changes and search.
- A custom API domain, access logging, alarms on 5xx or throttling, and the deployed upload and response-size boundary checks in [JMAP acceptance](../specs/jmap.md#acceptance-checks).
- Token expiry mid-stream and reconnect, and two concurrent CLI sessions ([clients](../specs/clients.md#acceptance-checks)).
