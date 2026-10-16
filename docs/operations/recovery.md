# Retention and recovery

[Documentation index](../README.md) · [Architecture](../architecture.md) · [Roadmap](../roadmap.md)

## Scope and status

**Status: proposed; implementation and validation are pending.** Owns permanent original retention, mailbox deletion, derived-data GC and the owner rebuild procedure. Search-specific lifecycle settings live in [search](../specs/search.md).

## Required behavior and invariants

- Original S3 mail objects and every version are retained permanently, including after mailbox deletion.
- Deletion removes the active Email and its normal blob/snippet access; it is not erasure of the archived original.
- Garbage collection applies only to temporary or derived data, never originals.
- No backup system is included in the current personal-use scope; metadata loss may be permanent.
- Rebuild never implicitly authorizes a send or bulk re-import; surviving deletion evidence is respected.

## Design

### Original mail retention

Keep originals in a dedicated, encrypted, versioned S3 bucket, separate from disposable objects. Originals include every raw SES delivery and durable MIME written for imported/composed mail or frozen submissions. Preserve the original bytes, including MIME attachments, and exact bucket/key/version references. Even failed or unprocessed deliveries, quarantined mail and synthetic canary deliveries remain in this archive. Missing metadata or reference counts never make an original eligible for deletion.

Enable versioning and retain both current and noncurrent versions without expiration or a version-count cap. No project runtime, maintenance, deployment or recovery role receives delete permissions on this bucket or its objects; the [IAM contract](../specs/security.md#iam-role-requirements) owns the exact restrictions. Do not configure lifecycle expiration for originals: bucket access permissions do not prevent S3 Lifecycle from expiring objects. See [S3 lifecycle behavior](https://docs.aws.amazon.com/AmazonS3/latest/userguide/lifecycle-expire-general-considerations.html).

Use unique keys and exact version references for originals rather than overwriting them during parsing, reprocessing or restore. Terraform protects the original bucket from destruction and never force-empties it. Permanent retention means storage usage continues to grow even when messages are deleted from clients; include retained original bytes/versions in the storage budget. See [S3 pricing](https://aws.amazon.com/s3/pricing/).

### Retention, deletion and garbage collection

| Data | Initial policy |
| --- | --- |
| Original raw/durable MIME and its S3 versions | Permanent retention under the policy above; mailbox deletion never deletes these objects |
| Live mailbox state | Keep until owner deletion; destroying an Email removes it from the active mailbox, not from the original archive |
| Trash/Junk/Quarantine | Keep visible until explicit owner deletion initially; review quarantine/DLQ alarms rather than auto-discarding accepted mail |
| JMAP ID-only change history and object tombstones | Follow the [history window, floor and consumer checkpoint contract](../specs/storage-sync.md#dynamodb-storage-and-consistency-contract); pending durable work is retained separately |
| Ingestion receipt/deletion markers | Minimal replay guards for the account lifetime; retain source identity/version references and deletion generation without message bodies or address projections |
| Temporary unattached uploads | Expire after 24 hours in the disposable bucket; before accepting an import, promote its complete durable MIME to the original bucket |
| Destroyed Email's derived data | Deny access immediately; remove unreferenced extracted attachments/body projections, residual search text and sensitive historical metadata within seven days; original MIME remains permanently |
| Search snapshots | Follow the [index version lifecycle](../specs/search.md#index-pipeline); mailbox deletion also purges snapshots containing deleted messages, regardless of lifecycle protection |
| Backups / scheduled exports | Not configured; original S3 versioning remains enabled, and DynamoDB PITR is disabled |
| Submission evidence | Retain attempt/submission audit and correlation records at least 35 days and until resolved; never replay them as a new send merely because a queue is replayed or metadata is rebuilt |

Email destruction commits a content-free deletion marker and cleanup outbox work together with removal of visible references. Normal JMAP get/query/search/download must not expose the retained original through an old ID. Retaining the original is not authorization to recreate the Email. Ordinary replay, reindex and rebuild respect surviving deletion markers; an intentional owner recovery/import is a separate explicit action with a new logical ID.

GC uses transactional reference counts/pins and a `deleting` claim for **disposable data only**. It may remove all versions of an eligible derived object, never versions in the original bucket. Imports/submissions cannot pin a disposable blob after that claim; they retry from a current snapshot or report an unavailable reference. Active send pins delay derived cleanup until resolution, with the exception reported to the owner. They do not affect the permanent retention of original MIME. Remove sensitive retained metadata projections within seven days, invalidating affected historical query snapshots when necessary; retain content-free sync/deletion evidence. This can shorten historical query availability without erasing the archive.

Publish a clean search snapshot from live Emails through the normal fence, then purge superseded snapshots containing destroyed Emails; do not rely on SQLite free-page deletion to remove text. Reader caches reject deleted snippets immediately and discard contaminated files within seven days. Rebuilds must start from live metadata and deletion evidence, not indiscriminately index every archived original. Parsed data, indexes and temporary uploads use separate storage/lifecycle policies from originals.

<a id="backup-and-export"></a>
### Recovery without backups

No backups are configured for the current personal-use scope: no additional S3 mail copies, backup bucket, scheduled metadata/content exports, AWS Backup plans, DynamoDB PITR or on-demand DynamoDB backups. Keep the existing original-object versioning and no-delete policies. Built-in S3 redundancy is the storage durability mechanism; there is no independent disaster-recovery copy.

**Accepted limitation:** original MIME can be read again, but it does not fully reconstruct mailbox state. Loss or corruption of DynamoDB can permanently lose read flags, folder membership, aliases/account mappings, deletion decisions, SMTP envelope recipients and submission/attempt evidence. Retained original bytes do not imply a recoverable metadata snapshot. No point-in-time recovery or metadata-loss target is promised in this scope.

When metadata remains intact, rebuild disposable parsed data, counters/listings and search from live Emails and exact retained original versions. When metadata is lost, rebuild a fresh account and let the owner explicitly select originals to import; do not silently restore the entire archive. Previously deleted mail can only be distinguished if deletion evidence survives. Raw MIME headers do not reliably recover SMTP envelope/BCC routing. Recovered originals receive new logical IDs when re-imported.

## Failure handling

<a id="restore-runbook"></a>
### Rebuild runbook

1. Pause outbound dispatch and destructive maintenance. Preserve new accepted inbound originals and queued receipt notifications while diagnosing the failure.
2. Recreate infrastructure/configuration from Terraform as needed, preserving the original S3 bucket and all versions. Use surviving metadata and exact original-version references when available; there is no backup restore step. Keep normal access disabled until consistency is checked.
3. If metadata survives, reconcile queued receipts and deletion markers, rebuild derived state, and keep intentionally destroyed Emails hidden. Leave unexplained archived objects outside the active mailbox until receipt/deletion evidence or explicit owner import resolves them.
4. If metadata is lost, initialize a fresh table/account mapping using independent owner credentials. Rotate the restore epoch/public JMAP account ID as needed and force full client discovery/sync. Re-import only owner-selected original messages; acknowledge that read flags, folders, deletion decisions and envelope metadata may be unavailable.
5. Treat active or uncertain sends as unresolved. Retained frozen MIME is not authorization to send again. Never dispatch reconstructed submission/outbox work or retry historical sends automatically; reconcile surviving attempt records with relay evidence and require an explicit new owner submission where needed.
6. Rebuild a clean search index from the resulting live account, run synthetic positive acceptance, then resume ingestion. Unpause sending separately after owner reconciliation. Record lost metadata and any arrivals that could not be reconciled.

Rehearse the rebuild with synthetic data before cutover and after storage/schema changes. This procedure recovers what remains available; it does not promise restoration of lost metadata or an earlier mailbox state.

## Acceptance checks

- With metadata intact, rebuild parsed data, counters/listings and search while preserving original object versions, deletion decisions and send-attempt evidence.
- With metadata deliberately removed from an isolated synthetic account, create a fresh account and explicitly re-import selected originals. Force client resync, demonstrate the expected loss of mutable state, and verify nothing is automatically re-sent or indiscriminately made visible.
- Test derived-data GC against in-flight imports and pinned send blobs. Verify expired uploads cannot be newly pinned after the deletion claim; test sensitive historical-metadata cleanup and index-version/cache cleanup.
- Destroy an Email, empty Trash, replay its original receipt and rebuild search: the Email stays inaccessible while its deletion evidence survives, and its original S3 version remains readable by an authorized owner recovery role.
- Validate that relevant roles lack original-object/version/bucket deletion grants, versioning is enabled, no original expiration rule exists, and derived GC cannot target the original bucket.
- Terraform checks require DynamoDB PITR to be disabled and no backup buckets, backup/export jobs, AWS Backup plans or backup-specific roles/grants to be provisioned.
