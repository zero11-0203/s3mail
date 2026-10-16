# Phase 0 evidence: SES receiving

[Documentation index](../README.md) · [Roadmap](../roadmap.md#phase-0--prove-the-risky-integrations)

**Status:** measured on 2026-10-17. This page records results, not requirements; they live in [mail flow](../specs/mail-flow.md#inbound-email), [retention](../operations/recovery.md#original-mail-retention) and [DNS](../operations/deployment.md#dns-and-region).

## Setup

- `infra/modules/mail-inbound` in `us-east-1`: SES domain identity verified by the `_amazonses` TXT record; one receipt rule set with a `deliver` rule (declared aliases plus `canary`, scanning on, S3 action with SNS topic, then a rule-set stop action). The MX, verification TXT and a `p=reject` DMARC record are declared by the module and published by the `dns` module.
- Original bucket: versioned, SSE-S3, `BucketOwnerEnforced`, public access blocked, no lifecycle configuration. Its bucket policy allows only SES `PutObject` under `inbound/` from this account and receipt rule, denies `DeleteObject`, `DeleteObjectVersion`, `DeleteBucket`, `PutLifecycleConfiguration` and `PutBucketVersioning` to every principal, and denies non-TLS requests.
- SNS topic → SQS ingestion queue (SSE-SQS, visibility 540 s for a planned 90 s Lambda, 14-day retention, DLQ after 5 receives). The subscription keeps the SNS envelope and sends undeliverable notifications to a separate delivery DLQ.
- No IAM role was created; SES and SNS act through resource policies with `AWS:SourceAccount`/`AWS:SourceArn` or `aws:SourceArn` conditions.
- The deployment domain had no live mail, so it served as the pilot domain itself rather than a subdomain; there was no primary MX routing to preserve.
- Probe: Python `smtplib` from a laptop on a residential connection straight to `inbound-smtp.us-east-1.amazonaws.com:25`, one synthetic message from `probe@example.com`.

## Results

| Check | Result |
| --- | --- |
| Public resolvers (1.1.1.1, 8.8.8.8) return MX, `_amazonses` TXT and DMARC | Pass |
| SES verification status after the TXT record was published | `Success` within 5 s |
| STARTTLS to the receiving endpoint | TLS 1.3 |
| `RCPT TO` an undeclared local part | Rejected in the SMTP transaction: `550 5.1.1 Requested action not taken: mailbox unavailable` |
| `RCPT TO` `<alias>+phase0` | Accepted (`250`) |
| `DATA` to the alias and its `+tag` variant, header `To:` naming only the alias | Accepted; one object of 1,929 bytes under `inbound/<SES message id>`, SSE `AES256`, with a version ID |
| Receipt notification on the ingestion queue | SNS `Notification` envelope; `notificationType: Received`; `receipt.recipients` lists both envelope recipients, including the one absent from the headers |
| Verdicts in the notification | spam `PASS`, virus `PASS`, SPF `FAIL`, DKIM `GRAY`, DMARC `FAIL` (expected for an unauthenticated `example.com` sender) |
| Lifecycle configuration on the original bucket | None |
| Owner admin session: `DeleteObject`, `PutBucketVersioning` (`Suspended`), `PutBucketLifecycleConfiguration` | Each rejected with `AccessDenied` from the explicit deny in the bucket policy |

The rule creation also wrote `inbound/AMAZON_SES_SETUP_NOTIFICATION`, which stays in the archive like any other object.

## Findings

1. **The S3-action notification has no version ID.** `receipt.action` carries `bucketName`, `objectKeyPrefix` and `objectKey` only. Ingestion has to resolve the version itself (the key is the unique SES message ID, so its first version is the delivery) before recording the reference; see [mail flow](../specs/mail-flow.md#inbound-email).
2. **TXT values are unquoted in the record contract.** The AWS provider adds the quotes; values passed already quoted were rejected by Route 53 as `InvalidCharacterString`. The `dns` module now rejects quoted TXT values.
3. **SES does not reject on DMARC.** A sender whose domain publishes `p=reject` was accepted with `dmarcVerdict: FAIL`, so the [disposition policy](../specs/mail-flow.md#recipient-and-processing-policy) has to act on the verdict after acceptance.
4. **Any principal with same-account `sqs:SendMessage` can inject a notification.** The queue policy limits SNS to the receipt topic, but identity policies in the account still apply, as the forged redelivery in the [ingestion round](#ingestion-round) showed. Ingestion only accepts notifications naming the receipt topic, the original bucket and a key equal to the prefix plus the message ID, and it reads the version that actually exists, so an injected notification can only reference a real delivery. Verifying the SNS signature would close the gap; until then, no project role is granted `SendMessage` on this queue.

## Ingestion round

Same day, after deploying the `ingest` Lambda (`go/cmd/ingest`, `provided.al2023` arm64, 256 MB, triggered through its `live` alias, one message per invocation, `ReportBatchItemFailures`) and the `s3mail-mail` table (`infra/modules/mail-store`, on-demand, PITR off, deletion protection on). The [permission inventory](../operations/permissions.md#s3mail-ingest-lambda-execution-role) lists its role.

| Check | Result |
| --- | --- |
| The first probe, still queued from the receiving round | Committed on its second receive: Quarantine, reason `dmarc` (the `example.com` sender publishes `p=reject`) |
| A second probe from a `.invalid` sender without DMARC | Committed to the Inbox |
| Records per delivery | One transaction wrote `STATE` (sequence guard), `RECEIPT#<SES id>`, `EMAIL#<id>`, `LIST#<mailbox>#<time>#<id>`, `CHANGE#<seq>` and two `OUTBOX#<seq>#…` entries |
| Exact original reference | The Email and receipt marker hold the bucket, key and version ID resolved through `ListObjectVersions` |
| Envelope routing | The Email holds both envelope recipients (`<alias>` and `<alias>+phase0`); the header `To:` named one |
| Receipt marker contents | Email ID, location, version, receipt time, sequence and parser version only; no sender, recipients or content |
| A forged redelivery of the first receipt (same SES message ID, sent to the queue by the owner's admin session) | Outcome `duplicate`, same Email ID, account sequence unchanged at 2 |
| Queues and DLQs afterwards | All empty; no `ERROR` or `WARN` log entries |

Deferred to Phase 1: mailbox, thread and counter records; `MESSAGEID#` lookups and real threading (each delivery is currently its own thread); `VERSION#` history; the outbox dispatcher (outbox entries are committed and wait); the full MIME parser with legacy charsets (the header projection currently marks those `incomplete: charset`); OpenTelemetry spans; and DLQ alarms.

## Not yet checked

- Messages near the 40 MB limit, a sender without STARTTLS, and the SNS delivery DLQ path.
- Contention between concurrent commits and a crash between commit and queue deletion, beyond the unit tests with a fake table.
