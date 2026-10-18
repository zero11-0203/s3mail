# Phase 3 gate audit

[Documentation index](../README.md) · [Roadmap](../roadmap.md#phase-3--recovery-and-primary-mail-cutover) · [Release gates](../roadmap.md#release-gates)

**Status:** audit on 2026-10-18 at commit `83bcc42`. This page classifies every core Phases 0–3 acceptance check against the evidence that exists. It records findings, not requirements: the checklists stay in their owning documents, and no gate is marked passed here. The Phase 4 browser gate and the agent phases are out of scope.

## Method

- Read each `## Acceptance checks` section that the [release gates](../roadmap.md#release-gates) table links, plus the specs' current-implementation notes and the other files in `docs/evidence/`.
- Matched each check to repository tests (`go test ./...`, `terraform test` in `infra/modules/*/tests`), commit notes and deployed state.
- Deployed state was read only through control-plane and metric reads: relay parameter, DynamoDB PITR status, bucket versioning, MX record, AWS Backup plans, Lambda invocation and error counts, and queue depths. No mail item, object or message body was read, and nothing was changed.

Classes:

- **Proven:** a test or a recorded deployed measurement covers the whole check.
- **Partial:** some of the check is covered; the gap is named.
- **Not done:** no evidence exists.
- **Owner decision:** the owner has declined it or may waive it, as with the A1 waiver. Waiving is recorded in the roadmap, not here.

## Deployed state seen

- The relay parameter names `smtp2go`, so sending is not paused. Over the last 14 days the submission worker ran 6 times, the relay webhook 16 times and the delivery worker 4 times, all without function errors. Every queue and DLQ was empty.
- The derived-data collector ran twice without errors. Work recovery runs every minute without errors.
- Both tables have PITR disabled, and the account has no AWS Backup plan. The originals and index buckets are versioned.
- The deployment domain's MX already points to SES receiving. The domain had no earlier mail ([SES receiving](phase0-ses-receiving.md#setup)), so "move primary MX" means starting to use these addresses as the owner's primary mail, not a DNS change.
- X-Ray received 75 traces in the last six hours.

## Results by gate

### Region, DNS/SES, binary/stream integration and pinned versions

[Deployment checks](../operations/deployment.md#acceptance-checks) and [JMAP checks](../specs/jmap.md#acceptance-checks).

| Check | Class | Evidence and gap |
| --- | --- | --- |
| Per-module `terraform test`: encryption, versioning, `prevent_destroy`, `live` alias, no NAT | Partial | Covered by `mail-inbound`, `mail-store`, `jmap-api` and others; `prevent_destroy` is set in `dns`, `auth`, `mail-inbound` and `mail-store`. No single test asserts "no NAT gateway" or the `live` alias for every function. |
| No backup resources, PITR disabled | Proven | `mail-store` `table_has_no_backups_and_is_protected`. Deployed: PITR disabled on both tables, no AWS Backup plan. |
| Destructive-change check against sample plan JSON | Not done | No plan checker exists in the repository or in CI. |
| DNS record set: complete, portable types, no apex CNAME | Partial | `dns` tests reject apex CNAME, alias types, duplicates and FQDN names; `mail-inbound` declares MX, verification and DMARC. Completeness of the merged set (SPF, relay DKIM, SRV, ACM) is not asserted. |
| Deliverability through the active relay: DKIM, SPF, DMARC pass | Not done | Live sends happened, but no external checker result is recorded. |
| Release failure injection, delayed canaries, coordinated rollback | Not done | No post-deploy CI. The canary address exists, but canary mail always goes to Quarantine because there are no nonce run records (`go/internal/ingest/routing.go`). |
| Streaming, upload boundary, pinned versions | Proven | [Streaming spike](phase0-streaming-spike.md): 15-minute streams, 4 MiB upload boundary, 25 MiB download, pinned Terraform, provider and Go runtime. |

### Alias, envelope, MIME disposition, relay, quotas and webhooks

[Mail flow checks](../specs/mail-flow.md#acceptance-checks).

| Check | Class | Evidence and gap |
| --- | --- | --- |
| Golden `.eml` corpus | Partial | `mailparse` tests cover nested multipart, inline parts, a missing closing boundary, Shift_JIS, ISO-2022-JP and RFC 2047. Not covered: GBK, Big5, EUC-KR, windows-1252, forwarded `message/rfc822`, DSNs and large attachments. There is no `.eml` corpus with expected output files. |
| Fuzzing of MIME, headers and the JMAP request parser; round-trip stability | Partial | `FuzzParse` covers the MIME parser only. There is no fuzz target for the JMAP request parser and no parse → serialize → parse check. |
| Alias and disposition routing, verdicts, bounds to Quarantine, retry and DLQ | Partial | `TestRoute`, `TestDisposition`, `TestParseHeadersBounds` and `TestHandleHeaderLimitQuarantines`. Deployed: unknown-recipient `550`, `+tag` and BCC envelope recipients ([SES receiving](phase0-ses-receiving.md#results)), and a DMARC quarantine. Missing: a nonce-bound canary and alias-registry deployment races. |
| Relay conformance: accepted, retryable, rejected, unknown; failure injection; Bcc; TLS; quotas | Partial | `relay` and `send` tests cover each outcome, an RCPT abort before DATA, Bcc stripping, refused plaintext, leases, cancellation and `TestUnknownIsNeverResent`; `TestReserve` covers quota reservation. Missing: concurrent reservations and period rollover under contention, and a recorded live Message-ID preservation check. |
| Bounce normalization and webhook authentication | Partial | `webhook` and `smtp2go` tests cover the bearer secret and event mapping; `send` `TestDeliveryEvents` and component `TestComponentDeliveryEvents` cover correlation and deduplication. Live webhooks ran without DLQ entries. No test shows that a forged inbound DSN cannot set delivery status. |

### Transactions, counters, state paging, history and outbox recovery

[Storage and sync checks](../specs/storage-sync.md#acceptance-checks).

| Check | Class | Evidence and gap |
| --- | --- | --- |
| Model-based property tests with two clients | Not done | No `rapid` or other property tests. Example-based tests (`TestEmailSetAndChanges`, `TestQueryChanges`, `TestHistoryFloor`, component tests) cover single paths only. |
| Concurrency injection | Partial | `store` tests cover transaction cancellation and sequence-conflict retry, and `TestCommitPreflightsTheItemLimit` covers the item limit. Missing: writes between STATE reads, concurrent moves and ingests, and unprocessed batch-get keys. |
| Original preservation after destroy, replay, retries and GC | Partial | The bucket policy denies deletes, which was probed live ([SES receiving](phase0-ses-receiving.md#results)), and `gc` tests never target originals. No end-to-end test destroys an Email, replays and collects, then checks the original version. |
| Idempotency, no resurrection, crash boundaries, no automatic resend | Partial | `TestHandleDuplicateSkipsReads`, `TestReceiptCancellationClassification`, the dispatch drain tests, `TestLeaseRecovery` and `TestUnknownIsNeverResent`. A live forged redelivery was refused (commit `fb45a7a`). Missing: crash injection at each commit and dispatch boundary. |

### Protocol matrix, push, limits and safe downloads

[JMAP checks](../specs/jmap.md#acceptance-checks).

| Check | Class | Evidence and gap |
| --- | --- | --- |
| RFC conformance suite | Partial | `jmap` tests plus the [RFC review](jmap-rfc-review.md), whose findings each have a regression test. These are in-process handler tests, not a black-box HTTP suite organized by RFC section. |
| Push: delivery, filters, ping, closeafter, heartbeats, leases, disable, reconnect | Partial | `TestEventSource` and `TestEventSourceEmailDelivery`; live `closeafter=state` ([aerc slice](phase0-aerc-slice.md#results)). Connection leases are not implemented, and reconnect at token expiry was not measured. |
| Deployed upload, body budgets, download headers, stream termination | Partial | The [streaming spike](phase0-streaming-spike.md) measured the boundaries on the spike routes, and the aerc slice checked download headers. The upload boundary has not been re-measured on the production `uploadUrl`. |
| Semantic search: off unless configured, limits, scope, live Emails only | Proven | `TestSemanticQueryArgumentsAndAvailability` and `TestSemanticQueryReturnsLiveEmailsOnly`; deployed on 2026-10-18. |

### Tokens, side effects, IAM roles and CI data access

[Security checks](../specs/security.md#acceptance-checks).

| Check | Class | Evidence and gap |
| --- | --- | --- |
| Authorization matrix | Partial | `auth` tests cover issuer, client, token use and exact scopes; `jmap` tests check scopes per method. Deployed: a missing or malformed token gets `401` with the discovery challenge. Missing: one table-driven matrix over every method and endpoint; tests for Basic credentials, another account, mixed-scope batches and the revocation or emergency-disable windows; and a CI run of the negative rows against AWS. |
| Telemetry privacy | Partial | `TestAWSSpansCarryNoMailData` covers span attributes. Logs from failing proxy, SDK, SMTP and webhook paths are not captured and checked. |
| CI can deploy but not read data | Not done | CI runs offline checks only (`.github/workflows/checks.yml`); no IAM test of data-plane denial exists. |
| IAM least privilege | Partial | Every module has a `least_privilege` (or equivalent) `terraform test`, and the [permission inventory](../operations/permissions.md) lists each grant. IAM Access Analyzer has not been run, and access activity has not been reviewed for unused grants. |
| Agent client | Proven | `auth` module tests and the agent review; this check belongs to the agent gate. |
| Original-delete denials | Partial | Deny probes by the owner session were recorded live. Single-object, batch and version-specific deletes by each runtime role were not tried. |

### Search completeness, publication, freshness, cost and retention

[Search checks](../specs/search.md#acceptance-checks).

| Check | Class | Evidence and gap |
| --- | --- | --- |
| CJK, NFKC, case folding | Proven | `TestNormalize` and `TestTokens`. |
| Replay of mutation sources with crash, reorder and duplicate wake-ups | Partial | `TestPipeline` and `TestIndexerRebuildsOnEpochChange`. Crash injection at each boundary is missing. |
| CAS races, WAL backup, registration crash, reader swap, live deletion checks | Partial | `TestComponentSearchRegistration` covers the conflict, `TestIndexMatchSnapshotAndDelete` the snapshot, and `TestTextQueryAndSnippets` the live check. Missing: two warm readers on different generations, a registration crash and the field-independence matrix. |
| 50,000-message benchmark, lag, latency, lifecycle, purge | Not done | There are no benchmarks. The purge has unit tests (`gc` `TestPurgeAfterDestroy`). |
| Semantic search | Proven | `vectors_test.go` and the search module test `semantic_reads_only_its_key`. The A4 evaluation was skipped by owner decision. |
| Keyword relevance set with recall@10 | Not done | No query set exists. |

### aerc/OAuth slice, response loss and two CLI sessions

[Clients checks](../specs/clients.md#acceptance-checks).

| Check | Class | Evidence and gap |
| --- | --- | --- |
| Client library and raw protocol drivers | Partial | Tested with aerc 0.22.0 live ([aerc slice](phase0-aerc-slice.md)) and `TestAercSendFlow`. The `go-jmap` release check is not recorded. |
| Recorded, scrubbed aerc sessions; two CLI sessions | Not done | No replay fixtures and no two-session run. |
| Owner acceptance checklist | Partial | Reading, `$seen` and push were recorded live. Live sends ran, but there is no recorded run of moves, drafts, attachments, Sent hooks, response-loss reconciliation or search from aerc. |

### Permanent originals, derived cleanup, owner access and rebuild

[Recovery checks](../operations/recovery.md#acceptance-checks).

| Check | Class | Evidence and gap |
| --- | --- | --- |
| Rebuild with metadata intact | Owner decision | `mail-admin rebuild-derived` exists with `TestComponentRebuildDerived`. The owner chose not to rehearse it ([rebuild runbook](../operations/recovery.md#rebuild-runbook)). |
| Rebuild without metadata, no automatic resend | Owner decision | Not rehearsed, by the same choice. `TestUnknownIsNeverResent` covers the resend rule in isolation. |
| GC against in-flight imports and pinned send blobs; cleanup | Partial | `gc` tests cover purge and history retention, and the collector ran live. Races with in-flight imports and pinned blobs are untested. |
| Destroy, empty Trash, replay, rebuild search; original still readable | Not done | Parts exist separately (`TestEmailDestroyAndThreads`, duplicate replay, the purge) but have not been run together. |
| No delete grants, versioning on, no expiration, GC cannot reach originals | Partial | Module tests and the live bucket probes. Versioning is enabled (seen deployed). A per-role effective-policy inspection is still missing. |
| PITR disabled, no backup resources | Proven | Same evidence as the deployment row. |

### Positive acceptance, nonce canary and rollback

This gate uses the [deployment checks](../operations/deployment.md#acceptance-checks). The positive acceptance, canary and rollback items are classified in the deployment table above: none is done.

### Mail-free telemetry, dashboard, budgets and tracing

[Monitoring checks](../operations/monitoring.md#acceptance-checks).

| Check | Class | Evidence and gap |
| --- | --- | --- |
| Logs, metrics and spans across failing paths; privacy | Partial | Span tests only, as in the security row. |
| Alarms, notification, dashboard links, missing recovery or GC completion | Owner decision | The owner chose no alarms. The dashboard exists (`ops` test `dashboard`). Missing GC completion is not detected. |
| OpenTelemetry reaches X-Ray, linked retries, bounded push traces | Partial | Traces reach X-Ray (75 in six hours), and one send is a single trace (commit `d2acf81`). Linked queue retries and push trace bounds were not checked. |
| Search and push costs against a budget | Not done | No budget is configured, and nothing was measured. |

## Summary

Of 45 checks, 7 are proven, 25 partial, 10 not done and 3 owner decisions. Most partial rows already have the unit tests; what they lack is a deployed run or failure injection. Nothing found contradicts an invariant: the originals are versioned and protected, there are no backups, unknown acceptance is never resent, and the DLQs are empty.

## What closes the gaps, in order

Ordered by risk to mail. The order follows the [deployment priority note](../operations/deployment.md#test-data-rules): the authorization matrix, sync properties and MIME corpus first.

1. **Deliverability check (owner, about 10 minutes).** Send one message through SMTP2GO from aerc to an external checker and record DKIM, SPF and DMARC results here. Do this before using the addresses as primary mail.
2. **Owner positive acceptance run (owner, with aerc).** Cover moves, drafts, an attachment, a Sent copy, search, and a second aerc or `jmap` session that sees the same changes. Record each result in an evidence file.
3. **Authorization matrix test (code).** One table-driven test over every method and endpoint with the token variants and missing scopes, run locally. Its tokenless rows can run against the deployed API.
4. **Sync property tests (code).** `rapid`-based model tests for ingest, move, keyword, destroy and changes with two clients.
5. **MIME corpus and fuzzing (code).** Add the missing charsets, DSNs and forwarded messages as `.eml` fixtures; add a JMAP request fuzz target.
6. **Original-preservation end-to-end (code, component tier).** Destroy, empty Trash, replay and collect, then assert the original version exists and the Email stays gone.
7. **Per-role delete denials and Access Analyzer (read-only AWS).** Run Access Analyzer policy validation on each role, and simulate the delete actions per role with `iam:SimulatePrincipalPolicy`.
8. **Deployed upload boundary on the production `uploadUrl`, and push reconnect at token expiry.**
9. **Destructive-change plan check (code).** A small checker for plan JSON, run before each `apply`, that refuses original-bucket lifecycle, versioning or delete-grant changes.
10. **Nonce canary, post-deploy checks and rollback procedure.** The largest item. A single owner could waive it for the cutover and keep the manual alias rollback.
11. **Search benchmark and keyword relevance set.** Matters at scale; the mailbox holds 11 Emails today.
12. **Monitoring gaps:** failing-path log privacy checks, a GC-completion signal and a cost budget.

Owner decisions:

- **Already declined:** the recovery rehearsals and alarms.
- **Candidates to waive:**
  - item 10 (canary and post-deploy CI) for a single-owner system;
  - item 11 until the mailbox grows;
  - the CI data-access test, while CI holds no AWS role (the CI workflow uses none).
