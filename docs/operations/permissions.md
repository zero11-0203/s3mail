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
| `CommitToOwnAccountPartition` | `dynamodb:GetItem`, `dynamodb:Query`, `dynamodb:BatchGetItem`, `dynamodb:PutItem`, `dynamodb:UpdateItem` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

**Exception:**
- `WriteTraces` uses `Resource: "*"` because X-Ray accepts no resource ARNs for these actions.

`Query` and `BatchGetItem` read the threading index, thread membership and mailbox/thread summaries the receipt transaction updates ([storage](../specs/storage-sync.md#dynamodb-storage-and-consistency-contract)).

**Excluded:**
- No `dynamodb:DeleteItem`: a receipt only creates records.
- No S3 write or delete of any kind, and no SMTP or other secrets.
- No index publication, IAM or `sts:AssumeRole`.
- The [shared-table limitation](../specs/security.md#iam-role-requirements) applies: inside its partition the role can write any record type.

### `s3mail-jmap-api` (Lambda execution role)

- **Module:** `infra/modules/jmap-api`
- **Purpose:** serve session discovery, JMAP methods and uploads, including the guarded commits of `Mailbox/set`, `Email/set` and `Email/import` ([JMAP](../specs/jmap.md), [conformance](../specs/jmap-conformance.md)).
- **Trusted principal:** `lambda.amazonaws.com`.

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-jmap-api` log group | — |
| `ReadAndUpdateOwnAccountPartition` | `dynamodb:GetItem`, `dynamodb:Query`, `dynamodb:BatchGetItem`, `dynamodb:PutItem`, `dynamodb:UpdateItem`, `dynamodb:DeleteItem` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `ReadOriginalVersions` | `s3:GetObjectVersion` | `<original bucket>/inbound/*` | — |
| `ReadNewOriginalVersions` | `s3:GetObjectVersion` | `<original bucket>/imported/*`, `<original bucket>/composed/*` | — |
| `ReadUploads` | `s3:GetObject` | `<blob bucket>/uploads/*` | — |
| `ArchiveNewOriginals` | `s3:PutObject` | `<original bucket>/imported/*`, `<original bucket>/composed/*` | — |
| `WriteUploads` | `s3:PutObject` | `<blob bucket>/uploads/*` | — |
| `InvokeSearch` | `lambda:InvokeFunction` | `s3mail-search-search` `live` alias | — |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

`DeleteItem` removes listing, membership, index and summary records when an Email moves or is destroyed; Emails leave a deletion marker and their original is never deleted. Archive writes use new ids with `If-None-Match: *`; the bucket policy's delete deny still applies. The same shared-table limitation as for ingest applies.

### `s3mail-jmap-download` (Lambda execution role)

- **Module:** `infra/modules/jmap-api`
- **Purpose:** stream an authorized blob of a live Email or an unexpired upload ([blobs](../specs/jmap.md#limits-and-blobs)).

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-jmap-download` log group | — |
| `ReadOwnAccountPartition` | `dynamodb:GetItem`, `dynamodb:BatchGetItem` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `ReadOriginalVersions` | `s3:GetObjectVersion` | `<original bucket>/inbound/*` | — |
| `ReadNewOriginalVersions` | `s3:GetObjectVersion` | `<original bucket>/imported/*`, `<original bucket>/composed/*` | — |
| `ReadUploads` | `s3:GetObject` | `<blob bucket>/uploads/*` | — |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

### `s3mail-jmap-push` (Lambda execution role)

- **Module:** `infra/modules/jmap-api`
- **Purpose:** poll account STATE for the EventSource ([push](../specs/jmap.md#push)).

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-jmap-push` log group | — |
| `ReadOwnAccountState` | `dynamodb:GetItem` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

### `s3mail-authorizer` (Lambda execution role)

- **Module:** `infra/modules/jmap-api`
- **Purpose:** verify Cognito access tokens for every JMAP route ([shared endpoint authorization](../specs/security.md#shared-endpoint-authorization)). It fetches the public JWKS over HTTPS without AWS credentials.

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-authorizer` log group | — |

**Excluded for all JMAP roles:**
- No S3 delete or list. Only the API role writes S3: new originals under `imported/` and `composed/`, and uploads; nothing can write `inbound/` except SES.
- No `dynamodb:Scan`, no other table and no other partition.
- No SQS, SNS, SES, secrets, IAM or `sts:AssumeRole`; only the API role invokes a function, the search alias.
- The download and push roles cannot write to the table; push cannot read S3.

### `s3mail-search-indexer` (Lambda execution role)

- **Module:** `infra/modules/search`
- **Purpose:** the single index writer (reserved concurrency 1): replay changes, read the originals of new Emails, publish snapshots with conditional PUTs and register each generation ([search](../specs/search.md#index-pipeline)).

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-search-indexer` log group | — |
| `ReadAndRegisterOwnAccountPartition` | `dynamodb:GetItem`, `dynamodb:Query`, `dynamodb:BatchGetItem`, `dynamodb:PutItem`, `dynamodb:UpdateItem` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `ReadOriginalVersions` | `s3:GetObjectVersion` | `<original bucket>/*` | — |
| `PublishIndex` | `s3:GetObject`, `s3:GetObjectVersion`, `s3:PutObject` | `<index bucket>/index/*` | — |
| `FindIndex` | `s3:ListBucket` | index bucket | — |
| `ConsumeWakeUps` | `sqs:ReceiveMessage`, `sqs:DeleteMessage`, `sqs:GetQueueAttributes` | `s3mail-index` queue | — |

A registration updates STATE and writes the SEARCH and one CHANGE record. `ListBucket` makes `HeadObject` on the not-yet-created index return 404 rather than 403; the bucket holds only index objects. The shared-table limitation applies.

### `s3mail-search-dispatcher` (Lambda execution role)

- **Module:** `infra/modules/search`
- **Purpose:** on a one-minute schedule, wake the indexer when Emails were created or destroyed after the registered generation.

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-search-dispatcher` log group | — |
| `ReadOwnAccountPartition` | `dynamodb:GetItem`, `dynamodb:Query` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `SendWakeUps` | `sqs:SendMessage` | `s3mail-index` queue | — |

### `s3mail-search-search` (Lambda execution role)

- **Module:** `infra/modules/search`
- **Purpose:** answer the API's text conditions and snippets from the registered generation.

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-search-search` log group | — |
| `ReadOwnAccountPartition` | `dynamodb:GetItem`, `dynamodb:Query` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `ReadIndexVersions` | `s3:GetObjectVersion` | `<index bucket>/index/*` | — |

**Excluded for all search roles:**
- No `dynamodb:DeleteItem`, no S3 delete of originals or index versions, no `dynamodb:Scan`.
- Only the indexer writes (the table and the index bucket) and only it reads mail content; the dispatcher and search roles cannot read originals.
- No SES, SNS, secrets, IAM or `sts:AssumeRole`.

## Resource policies

| Resource | Principal | Grant | Condition |
| --- | --- | --- | --- |
| Original bucket | `ses.amazonaws.com` | `s3:PutObject` on `inbound/*` | `AWS:SourceAccount` = this account, `AWS:SourceArn` = the `deliver` receipt rule |
| Original bucket | `*` | **Deny** `s3:DeleteObject`, `s3:DeleteObjectVersion`, `s3:DeleteBucket`, `s3:PutLifecycleConfiguration`, `s3:PutBucketVersioning` | — |
| Original bucket | `*` | **Deny** `s3:*` | `aws:SecureTransport` = false |
| Blob bucket | `*` | **Deny** `s3:*` | `aws:SecureTransport` = false |
| Index bucket | `*` | **Deny** `s3:*` | `aws:SecureTransport` = false |
| Receipt SNS topic | `ses.amazonaws.com` | `SNS:Publish` | `AWS:SourceAccount`, `AWS:SourceArn` as above |
| Ingestion queue, delivery DLQ | `sns.amazonaws.com` | `sqs:SendMessage` | `aws:SourceArn` = receipt topic |
| Ingestion DLQ | — | Redrive allowed only from the ingestion queue | — |
| `s3mail-jmap-api` `live` alias | `apigateway.amazonaws.com` | `lambda:InvokeFunction` | `AWS:SourceAccount`; source ARN `<api>/*/GET/.well-known/jmap`, `<api>/*/POST/jmap` and `<api>/*/POST/upload/*` |
| `s3mail-jmap-download` `live` alias | `apigateway.amazonaws.com` | `lambda:InvokeFunction` | `AWS:SourceAccount`; source ARN `<api>/*/GET/download/*` |
| `s3mail-jmap-push` `live` alias | `apigateway.amazonaws.com` | `lambda:InvokeFunction` | `AWS:SourceAccount`; source ARN `<api>/*/GET/eventsource` |
| `s3mail-search-dispatcher` `live` alias | `events.amazonaws.com` | `lambda:InvokeFunction` | source ARN = the `s3mail-search-dispatch` schedule rule |
| Index DLQ | — | Redrive allowed only from the index queue | — |
| `s3mail-authorizer` | `apigateway.amazonaws.com` | `lambda:InvokeFunction` | `AWS:SourceAccount`; source ARN `<api>/authorizers/<bearer>` |

Same-account identities with IAM permissions, such as the owner's admin session, can still send to the queues; see [the evidence finding](../evidence/phase0-ses-receiving.md#findings).

## Not yet created

- CI plan, code-deploy and infrastructure-deploy roles, and the permissions boundary ([CI identity](../specs/security.md#ci-identity)).
- The outbox dispatcher, submission, webhook, GC and recovery roles. Uploads run in the API role.
- The Phase 0 spike roles (`spike-stream` and its authorizer) were destroyed with the spike.
