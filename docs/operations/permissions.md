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
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

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
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

A registration updates STATE and writes the SEARCH and one CHANGE record. `ListBucket` makes `HeadObject` on the not-yet-created index return 404 rather than 403; the bucket holds only index objects. The shared-table limitation applies.

### `s3mail-search-search` (Lambda execution role)

- **Module:** `infra/modules/search`
- **Purpose:** answer the API's text conditions and snippets from the registered generation.

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-search-search` log group | — |
| `ReadOwnAccountPartition` | `dynamodb:GetItem`, `dynamodb:Query` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `ReadIndexVersions` | `s3:GetObjectVersion` | `<index bucket>/index/*` | — |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

**Excluded for all search roles:**
- No `dynamodb:DeleteItem`, no S3 delete of originals or index versions, no `dynamodb:Scan`.
- Only the indexer writes (the table and the index bucket) and only it reads mail content; the search role cannot read originals.
- No SES, SNS, secrets, IAM or `sts:AssumeRole`.

### `s3mail-outbound-dispatcher` (Lambda execution role)

- **Module:** `infra/modules/mail-outbound`
- **Purpose:** drain the transactional outbox: triggered by the table's KEYS_ONLY stream filtered to `OUTBOX#` inserts, plus a five-minute sweep that also checks the index against the change log ([storage](../specs/storage-sync.md#idempotency-and-durable-work)).

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-outbound-dispatcher` log group | — |
| `DrainOwnAccountOutbox` | `dynamodb:GetItem`, `dynamodb:Query`, `dynamodb:DeleteItem` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `ReadTableStream` | `dynamodb:DescribeStream`, `dynamodb:GetRecords`, `dynamodb:GetShardIterator`, `dynamodb:ListStreams` | the table's stream | — |
| `SendWakeUps` | `sqs:SendMessage` | `s3mail-index` and `s3mail-submit` queues | — |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

`DeleteItem` acknowledges outbox entries after their destination accepted the work; the code deletes only `OUTBOX#` keys, but IAM cannot restrict sort-key prefixes, so the shared-table limitation applies. The stream carries keys only, never mail content.

### `s3mail-outbound-worker` (Lambda execution role)

- **Module:** `infra/modules/mail-outbound`
- **Purpose:** claim, hand off and record submission attempts through the relay adapter ([outbound email](../specs/mail-flow.md#outbound-email)). The only role that reads the relay secret or talks to the relay.

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-outbound-worker` log group | — |
| `ReadAndCommitOwnAccountPartition` | `dynamodb:GetItem`, `dynamodb:Query`, `dynamodb:BatchGetItem`, `dynamodb:PutItem`, `dynamodb:UpdateItem` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `ReadRelayParameter` | `ssm:GetParameter` | `/s3mail/relay` parameter | — |
| `ReadRelaySecret` | `secretsmanager:GetSecretValue` | `s3mail/relay/smtp2go` secret | — |
| `ReadSendMaterial` | `s3:GetObjectVersion` | `<original bucket>/composed/*`, `<original bucket>/imported/*` | — |
| `ConsumeAndRequeue` | `sqs:ReceiveMessage`, `sqs:DeleteMessage`, `sqs:GetQueueAttributes`, `sqs:SendMessage` | `s3mail-submit` queue | — |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

### `s3mail-outbound-recovery` (Lambda execution role)

- **Module:** `infra/modules/mail-outbound`
- **Purpose:** the one-minute work-recovery sweep: settle expired attempt leases and re-enqueue due submissions whose wake-up was lost.

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-outbound-recovery` log group | — |
| `ReadAndCommitOwnAccountPartition` | `dynamodb:GetItem`, `dynamodb:Query`, `dynamodb:BatchGetItem`, `dynamodb:PutItem`, `dynamodb:UpdateItem` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `ReadRelayParameter` | `ssm:GetParameter` | `/s3mail/relay` parameter | — |
| `SendWakeUps` | `sqs:SendMessage` | `s3mail-submit` queue | — |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

### `s3mail-outbound-webhook` (Lambda execution role)

- **Module:** `infra/modules/mail-outbound`
- **Purpose:** the public relay webhook receiver behind `POST /webhooks/smtp2go`: check the bearer secret and persist the body on the delivery queue.

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-outbound-webhook` log group | — |
| `ReadWebhookSecret` | `secretsmanager:GetSecretValue` | `s3mail/relay/smtp2go-webhook` secret | — |
| `PersistNotifications` | `sqs:SendMessage` | `s3mail-delivery` queue | — |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

It has no table, mail, relay-credential or other queue access; reserved concurrency 2 bounds abuse of the unauthenticated route.

### `s3mail-outbound-delivery` (Lambda execution role)

- **Module:** `infra/modules/mail-outbound`
- **Purpose:** apply queued delivery reports to submission delivery status with guarded commits.

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-outbound-delivery` log group | — |
| `ReadAndCommitOwnAccountPartition` | `dynamodb:GetItem`, `dynamodb:Query`, `dynamodb:BatchGetItem`, `dynamodb:PutItem`, `dynamodb:UpdateItem` | `s3mail-mail` table | `dynamodb:LeadingKeys` = `ACCOUNT#<account>` |
| `ConsumeNotifications` | `sqs:ReceiveMessage`, `sqs:DeleteMessage`, `sqs:GetQueueAttributes` | `s3mail-delivery` queue | — |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

**Excluded for all outbound roles:**
- No S3 write or delete; only the worker reads originals, by exact version and only under `composed/` and `imported/`.
- Only the worker reads the relay credential secret and only the webhook receiver reads the webhook secret; no role can write a secret or the relay parameter (all are set out of band).
- Only the dispatcher deletes table items. Worker, recovery and delivery commits write STATE, CHANGE, SUBMISSION, ATTEMPT, QUOTA and OUTBOX records and never delete; attempt records are append-only.
- No `dynamodb:Scan`, SES, SNS, IAM or `sts:AssumeRole`.

### `s3mail-webmail` (Lambda execution role)

- **Module:** `infra/modules/webmail`, applied only by the separate `infra/webmail` root (Phase 4).
- **Purpose:** run the pinned webmail server behind Lambda Web Adapter ([browser hosting](../specs/clients.md#browser-hosting-and-client-safety)). It exchanges OAuth codes and refresh tokens with Cognito over public HTTPS; mail is reached only from the browser with the user's token.

| Statement | Actions | Resource | Condition |
| --- | --- | --- | --- |
| `WriteOwnLogs` | `logs:CreateLogStream`, `logs:PutLogEvents` | `/aws/lambda/s3mail-webmail` log group | — |
| `WriteTraces` | `xray:PutTraceSegments`, `xray:PutTelemetryRecords` | `*` | — |

**Excluded:** any DynamoDB, S3, SQS, SNS, SES, Lambda invoke, Secrets Manager, ECR, IAM or `sts:AssumeRole` grant. The image is pulled by the Lambda service under the repository policy below, not by this role.

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
| Index DLQ | — | Redrive allowed only from the index queue | — |
| `s3mail-outbound-dispatcher` `live` alias | `events.amazonaws.com` | `lambda:InvokeFunction` | source ARN = the `s3mail-outbound-dispatcher` schedule rule |
| `s3mail-outbound-recovery` `live` alias | `events.amazonaws.com` | `lambda:InvokeFunction` | source ARN = the `s3mail-outbound-recovery` schedule rule |
| Submission DLQ | — | Redrive allowed only from the submission queue | — |
| `s3mail-outbound-webhook` `live` alias | `apigateway.amazonaws.com` | `lambda:InvokeFunction` | `AWS:SourceAccount`; source ARN `<api>/*/POST/webhooks/smtp2go` (no gateway authorizer; the function checks the bearer secret) |
| Delivery DLQ | — | Redrive allowed only from the delivery queue | — |
| `s3mail-authorizer` | `apigateway.amazonaws.com` | `lambda:InvokeFunction` | `AWS:SourceAccount`; source ARN `<api>/authorizers/<bearer>` |
| `s3mail-webmail` `live` alias (Phase 4) | `apigateway.amazonaws.com` | `lambda:InvokeFunction` | `AWS:SourceAccount`; source ARN `<webmail api>/*/*` (no gateway authorizer; the server's own session checks protect the application) |
| `s3mail-webmail` ECR repository (Phase 4) | `lambda.amazonaws.com` | `ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer` | `aws:sourceArn` like `arn:aws:lambda:<region>:<account>:function:s3mail-webmail*` |

Same-account identities with IAM permissions, such as the owner's admin session, can still send to the queues; see [the evidence finding](../evidence/phase0-ses-receiving.md#findings).

## Not yet created

- CI plan, code-deploy and infrastructure-deploy roles, and the permissions boundary ([CI identity](../specs/security.md#ci-identity)).
- The GC and owner-recovery roles. Uploads run in the API role.
- The Phase 0 spike roles (`spike-stream` and its authorizer) were destroyed with the spike.
