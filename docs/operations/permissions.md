# Permission inventory

[Documentation index](../README.md) · [Security](../specs/security.md#iam-role-requirements)

**Status:** lists what is deployed now. The rules each entry must meet live in the [IAM requirements](../specs/security.md#iam-role-requirements); this page is the checked-in inventory they ask for. Update it in the same change as any role, policy or resource-policy grant.

## Roles

### `s3mail-ingest` (Lambda execution role)

- **Module:** `infra/modules/mail-inbound`
- **Purpose:** consume the ingestion queue, read the delivered original and commit the receipt transaction ([mail flow](../specs/mail-flow.md#inbound-email)).
- **Trusted principal:** `lambda.amazonaws.com`.

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-ingest` log group | — |
| `ConsumeIngestQueue` | `sqs:ReceiveMessage`, `sqs:DeleteMessage`, `sqs:GetQueueAttributes` | `s3mail-ingest` queue | — |
| `ReadOriginalVersions` | `s3:GetObjectVersion` | `<original bucket>/inbound/*` | — |
| `ListOriginalVersions` | `s3:ListBucketVersions` | original bucket | `s3:prefix` like `inbound/*` |
| `CommitToOwnAccountPartition` | `dynamodb:GetItem`, `dynamodb:PutItem`, `dynamodb:UpdateItem` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

**Exception:**
- `WriteTraces` uses `Resource: "*"` because X-Ray accepts no resource ARNs for these actions.

**Excluded:**
- No S3 write or delete of any kind, and no SMTP or other secrets.
- No index publication, IAM or `sts:AssumeRole`.
- The [shared-table limitation](../specs/security.md#iam-role-requirements) applies: inside its partition the role can write any record type.

## Resource policies

| Resource | Principal | Grant | Condition |
| --- | --- | --- | --- |
| Original bucket | `ses.amazonaws.com` | `s3:PutObject` on `inbound/*` | `AWS:SourceAccount` = this account, `AWS:SourceArn` = the `deliver` receipt rule |
| Original bucket | `*` | **Deny** `s3:DeleteObject`, `s3:DeleteObjectVersion`, `s3:DeleteBucket`, `s3:PutLifecycleConfiguration`, `s3:PutBucketVersioning` | — |
| Original bucket | `*` | **Deny** `s3:*` | `aws:SecureTransport` = false |
| Receipt SNS topic | `ses.amazonaws.com` | `SNS:Publish` | `AWS:SourceAccount`, `AWS:SourceArn` as above |
| Ingestion queue, delivery DLQ | `sns.amazonaws.com` | `sqs:SendMessage` | `aws:SourceArn` = receipt topic |
| Ingestion DLQ | — | Redrive allowed only from the ingestion queue | — |

Same-account identities with IAM permissions, such as the owner's admin session, can still send to the queues; see [the evidence finding](../evidence/phase0-ses-receiving.md#findings).

## Not yet created

- CI plan, code-deploy and infrastructure-deploy roles, and the permissions boundary ([CI identity](../specs/security.md#ci-identity)).
- The API, dispatcher, indexer, submission, webhook, GC and recovery roles.
- The Phase 0 spike roles (`spike-stream` and its authorizer) were destroyed with the spike.
