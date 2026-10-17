# Account storage and synchronization

[Documentation index](../README.md) · [Architecture](../architecture.md) · [Roadmap](../roadmap.md)

## Scope and status

**Status: proposed; implementation and validation are pending.** Owns the account model, DynamoDB commit protocol, state/history and durable work. Alias routing lives in [mail flow](mail-flow.md); search publication lives in [search](search.md).

## Required behavior and invariants

- One owner maps to one JMAP account; aliases are identities, not accounts.
- Each committed mutation atomically preserves objects, counters, state, history and required work.
- A state-bearing response must describe one coherent source boundary.
- Duplicate receipt delivery cannot resurrect mail destroyed in JMAP.
- Archived original MIME is retained permanently under the [retention policy](../operations/recovery.md#original-mail-retention); JMAP destruction only removes active references and derived data.

## Design

### Account model

- **Cognito user:** the one human owner who logs in.
- **JMAP account:** the one logical mailbox account, mapped from the Cognito subject (`sub`).
- **Email aliases / sending identities:** addresses belonging to that account; optionally enabled independently for receiving and sending.
- **Mailboxes:** Inbox, Sent, Drafts, Archive, Junk, Trash and Quarantine, plus optional organizational folders. Quarantine is an ordinary restricted-disposition folder, without a special JMAP role.
- **Messages:** stored centrally and visible consistently across all clients.

Example addresses:

| Email address | Purpose | Placement |
| --- | --- | --- |
| `me@example.com` | Primary correspondence | Unified Inbox |
| `shopping@example.com` | Purchases and receipts | Unified Inbox; optional Shopping mailbox |
| `github@example.com` | Development notifications | Unified Inbox; optional Notifications mailbox |
| `newsletter@example.com` | Subscriptions | Unified Inbox; optional Newsletters mailbox |

Store the actual SMTP envelope recipient(s) for each inbound delivery; MIME `To` headers alone are insufficient for BCC, aliases or forwarded mail. For replying, prefer the address that actually received the message, subject to the configured sending identities.

### Representative internal types

```typescript
type EmailAlias = {
  address: string;
  accountId: string;
  canReceive: boolean;
  canSend: boolean;
  defaultMailboxId?: string;
  isActive: boolean;
};

type StoredEmail = {
  id: string;
  accountId: string;
  s3Key: string;
  deliveredTo: string[];
  mailboxIds: Record<string, true>;
  threadId: string;
};
```

These are internal model sketches, **not** exact wire-format JMAP objects. The server should implement the published JMAP object schemas and synchronization semantics.

### DynamoDB storage and consistency contract

The mail table uses `PK = ACCOUNT#<internalAccountId>` and typed sort keys. This deliberately serializes commits for one owner; no GSI is used for a correctness-critical JMAP read. MIME and large parsed structures stay in S3, keeping each DynamoDB item below 400 KiB.

| Sort key / record | Purpose and read path |
| --- | --- |
| `STATE` | Account commit sequence, restore epoch, per-type state, retained-history floor and search publication metadata |
| `EMAIL#id`, `MAILBOX#id`, `IDENTITY#id`, `SUBMISSION#id` | Authoritative objects; strongly consistent get/batch-get, retrying unprocessed keys. System mailboxes are fixed defaults that a `MAILBOX#` record only overrides |
| `LIST#mailboxId#receivedAt#emailId` | Strongly consistent mailbox/date listing, with a stable email-id tie-breaker |
| `MEMBER#threadId#receivedAt#emailId` | Thread members, oldest first; a Thread is exactly its members. Stable thread ID chosen at email creation |
| `MAILBOXTHREAD#mailboxId#threadId`, `COUNT#mailboxId` | Transactional mailbox/thread count summaries and the mailbox counters derived from them |
| `MESSAGEID#hash#emailId`, `SUBJECT#hash#emailId` | Normalized Message-ID/reference and reply base-subject lookup entries for thread assignment |
| `CHANGE#<padded-sequence>` | Ordered durable record of changed object IDs/types, destruction and search-dirty IDs; one record per commit |
| `VERSION#type#id#<padded-sequence>` | Retained metadata versions for supported historical query evaluation; same history floor as changes |
| `RECEIPT#sesMessageId`, `DELETED#emailId` | Minimal durable ingestion/replay and deletion markers, independent of visible email lifetime |
| `OUTBOX#<padded-sequence>#destination` | Pending queue/event publication, committed with the source mutation; `trace` holds the committing request's X-Ray header, when traced, so dispatch continues its trace |
| `SEARCH` | Registered search-index generation: id, source sequence, epoch, object key, version and ETag; replaced only by a commit naming the previous generation ([search](search.md#index-pipeline)) |
| `BLOB#id`, `ATTEMPT#id`, `QUOTA#relay#period` | Blob ownership/reference state, send recovery and atomic quota reservations |
| `RELAYID#relay#queueId`, `EVENT#relay#eventId` | Delivery-report correlation from the relay's queue id to its submission, and the deduplication marker of each applied report ([webhook idempotency](mail-flow.md#webhook-idempotency)) |

Every authoritative mutation reads `STATE` and its affected objects consistently, then uses one `TransactWriteItems` to condition on the previous sequence and commit objects/retained versions, listing entries, counters, change record, required outbox entries and next sequence together. Attempts, quotas, blob pins and internal outbox bookkeeping follow the same guard; internal bookkeeping records are not search-dirty and do not recursively create new outbox work. Retry a sequence conflict from a fresh read with bounded jitter. Deduplicate actions on the same item when constructing the transaction. Mailbox counts include unique email counts and distinct total/unread thread counts, calculated from transactional per-mailbox/thread membership summaries. Thread membership is fixed at creation using normalized References/In-Reply-To lookups, then subject/address fallback; a later message never renumbers an existing thread. Parent links must remain acyclic.

For a state-bearing read, read `STATE` strongly before and after the strongly consistent object/list reads. Return only if the account sequence and restore epoch agree; otherwise retry up to three times and return a retryable error before exposing partial results. This provides a consistent boundary despite concurrent ingestion and client writes. General metadata queries initially scan the relevant typed records at personal scale and apply filtering/sorting in Go; optimize only with indexes maintained in the same commit protocol. Time-bounded queries fail explicitly rather than silently returning truncated matches.

State tokens encode the restore epoch, data type and its sequence; a type's state may advance conservatively. Retain change history and object tombstones for at least 30 days. A maintenance job advances a history floor under the same sequence guard before deleting older history; TTL alone never defines availability. `*/changes` fixes an upper sequence for each successful page, coalesces IDs into created/updated/destroyed relative to the requested state, and never advances past an unreported change. Expired or foreign-epoch states return `cannotCalculateChanges`. Retain historical versions needed for supported metadata `queryChanges` for the same window; unsupported filter/sort combinations advertise `canCalculateChanges: false`. Query states include normalized filter/sort, restore epoch and the applicable metadata/index revision. See [JMAP state and query methods](https://www.rfc-editor.org/rfc/rfc8620.html#section-5.2).

Advertise only the [JMAP object and membership limits](jmap.md#limits-and-blobs) proven by the transaction builder. Each set operation succeeds or fails per object; preflight its transaction against DynamoDB's 100-item/4 MiB limits and the [MIME header/metadata limits](mail-flow.md#recipient-and-processing-policy). Check `ifInState` before mutation and guard the first commit against that state; do not return a method-level retry error after partially applying a set. Return per-object outcomes and accurate old/new states. Delete a nonempty mailbox only through explicit emptying/moving batches followed by a final guarded delete; conflicting references prevent deletion. The transaction builder and concurrency tests must prove the worst-case membership/counter fan-out fits before these limits are published.

### Idempotency and durable work

SNS/SQS notifications and work queues deliver at least once. Every handler distinguishes duplicate work, safe retry and an uncertain external side effect.

- **Deterministic inbound IDs:** derive a URL-safe email ID from the account and SES message ID. Preserve the notified S3 bucket/key/version separately, including any configured key prefix. A retried receipt resolves to the same logical email and storage keys; client creates/imports use their own new IDs.
- **Durable receipt marker:** ingestion conditions on absence of `RECEIPT#sesMessageId`, not absence of the visible Email. Commit the receipt marker, email, counters, change record and outbox atomically. The minimal marker remains after permanent deletion for the lifetime of this account, with no sender, recipients or content after retention expires. Supported replay cannot resurrect intentionally deleted mail. A deliberate re-import uses a new logical ID. The retained original S3 object is not itself a live Email or permission to restore one automatically.
- **Classify transaction cancellation:** inspect cancellation reasons and strongly read the receipt marker before declaring a duplicate. A sequence/version conflict retries; throttling and transient failures retry; validation failures are quarantined/diagnosed. Never treat an arbitrary failed condition as proof that the email committed.
- **Transactional outbox for every mutation:** ingestion, JMAP create/import/set/destroy, submission hooks and delivery updates commit destination work with their source change. A DynamoDB Streams notification (a KEYS_ONLY stream filtered to outbox inserts) wakes the dispatcher, and a five-minute sweep queries the outstanding outbox to recover missed wake-ups; submissions are additionally re-enqueued by the one-minute [work recovery](mail-flow.md#outbound-email). The dispatcher removes an entry with a plain delete once its destination accepted the work: entries are immutable internal bookkeeping, not JMAP-visible state. Mark/remove work only after destination acceptance; inspect per-entry failures from batch/event APIs. A crash can duplicate a publication but cannot lose the committed intent. Keep pending entries until resolved; SQS queues/DLQs retain work for 14 days and alarm immediately. This sweep is operational recovery, not a scheduled test.

## Failure handling

Sequence conflicts restart from a fresh read; bounded read retries fail explicitly. Receipt-condition failures must be classified using the durable marker. Outbox retries preserve intent until destination acceptance. Expired history requires full resynchronization; [search](search.md#index-pipeline) owns index-history gap recovery. [Recovery](../operations/recovery.md) owns original retention, derived-cleanup exceptions and restore epochs.

## Acceptance checks

- **Model-based property tests for sync** (e.g. `pgregory.net/rapid`): generate ingest, keyword/move/destroy, mailbox and submission-hook sequences with two clients and compare against an in-memory reference model. Replay from every supported retained state must reproduce current state; older/foreign-epoch states must produce the defined full-resync error. Supported `queryChanges` must agree with a fresh query. Test deletion between change pages and history-floor advancement.
- **Concurrency:** inject writes between the before/after STATE reads, concurrent moves/ingests, stale fake index reads, transaction conflicts and unprocessed batch-get keys. Assert no state/result mismatch or lost update, correct unique thread/email counters and the worst-case 100-action/4 MiB transaction bound.
- **Original preservation:** after Email destruction, receipt replay, parsing retries and derived GC, assert original object versions still exist; normal JMAP blob access remains denied.
- **Idempotency:** duplicate SES notifications count once; ingest → permanent delete → original event replay cannot resurrect mail. Inject state-condition failures separately from duplicate-receipt conditions. Crash after commit/before dispatch and after destination acceptance/before marking outbox completion; every committed intent eventually reaches its destination. Live send leases remain live on duplicate work; expired handoff attempts become unknown and are never automatically resent.
