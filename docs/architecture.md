# Mail system architecture

[Documentation index](README.md)

**Status:** implemented and running on a reference deployment for validation; not every acceptance check has passed yet (see the [limitations](../README.md#limitations)).

## Overview

A self-hosted, single-owner email system on AWS: SES receives mail, a pluggable SMTP relay sends it, S3 permanently retains original messages, DynamoDB owns metadata and synchronization state, and a Go Lambda API serves JMAP. Cognito supplies the shared OAuth identity; Terraform manages infrastructure and DNS. The backend works independently of any client or AI feature.

**One owner, one JMAP account, many addresses, one unified inbox.** Aliases select receiving and sending identities rather than separate logins. aerc is the first client for development, testing and initial production use. Browser webmail has lower priority and is deferred, using an on-demand Lambda container with AWS Lambda Web Adapter. The core system and primary-mail cutover do not depend on browser deployment.

This page explains the system. The [topic specifications](README.md#topic-specifications) own detailed requirements and acceptance checks; [operations guides](README.md#operations) own deployment and recovery procedures.

## Scope

The initial system receives and sends custom-domain mail, supports multiple aliases with shared message state, and implements the required JMAP Core, Mail and Submission behavior. Existing OAuth-capable clients provide the UI. Account-scoped authorization, modest operational overhead and repeatable infrastructure are core goals.

Initial non-goals are multi-user hosting, a custom client or aerc fork, IMAP/SMTP submission compatibility, password/Basic/app-password authentication on mail endpoints, CalDAV/CardDAV and AI features in the core system (the mail agent is a separate, optional deployment). An IMAP gateway is a possible later extension.

The interoperable JMAP server is built in Go. Go covers the backend and terminal helpers; AWS provides managed transport, storage and compute; Terraform provides repeatable provisioning. The repository must contain no secrets, account IDs, real addresses or personal mail: examples use `example.com`, and private Terraform values stay gitignored. Python, LangGraph, AgentCore and an external model API (OpenRouter) are used only by the optional, separately deployed [mail agent](specs/agent.md) and the optional semantic search.

## Components and data flow

![AWS mail-system architecture: CLI-first mail paths, the separately deployed browser webmail and the mail agent](diagrams/aws-mail-system.drawio.svg)

[Open the editable AWS diagram](diagrams/aws-mail-system.drawio.svg) · [Editing and maintenance](diagrams/README.md)

This is a logical diagram. SES writes raw MIME and publishes the **S3 action receipt notification**, preserving envelope recipients and verdicts through SNS/SQS. Bare S3 notifications are insufficient. The domain stays at its registrar, with authoritative DNS delegated to Route 53. Lambdas use public AWS service endpoints without a VPC/NAT gateway; producer/account restrictions protect service integrations.

Incoming mail is durably parsed and committed with state/history/outbox work. JMAP clients read and mutate that same authoritative state. A dispatcher delivers committed work to submission and indexing queues. Submission workers retain attempt evidence across failures; uncertain relay acceptance requires owner reconciliation instead of automatic resend.

Original mail lives in a dedicated versioned S3 bucket whose relevant roles have no delete permissions. Deleting an Email removes active mailbox/search access while retaining its original; disposable uploads and derived data live separately. The [retention policy](operations/recovery.md#original-mail-retention) defines the boundary. No additional backups or DynamoDB PITR are configured in the current scope; loss of mailbox metadata may be permanent.

A single indexer publishes coherent SQLite FTS5 snapshots in S3; a separate search Lambda reads registered versions. Search is rebuildable and does not determine mailbox listing or synchronization correctness. Optional [semantic search](specs/search.md#semantic-search) adds embeddings from OpenRouter to the same snapshots; it is off by default. Shared authorization covers all public JMAP routes, including streamed downloads and EventSource. The deferred webmail uses a separate gateway and its own browser session/login handling.

## Component responsibilities

| Component | Responsibility / detailed contract |
| --- | --- |
| SES → SNS/SQS → Go ingestion | [Envelope routing, aliases, MIME bounds and disposition](specs/mail-flow.md) |
| S3 + DynamoDB | [Content references, account model, transactions, counters and sync history](specs/storage-sync.md) |
| REST API Gateway + Go Lambdas | [JMAP methods, limits, push and blob streaming](specs/jmap.md) |
| Cognito + IAM | [User authorization and least privilege for every created role](specs/security.md) |
| Outbox + submission worker + SMTP2GO adapter | [Durable delivery, quotas and bounce correlation](specs/mail-flow.md#outbound-email) |
| SQLite FTS5 on S3 + indexer/search Lambdas | [Multilingual search, consistent publication and bounded version retention](specs/search.md) |
| aerc + mail-auth; deferred Next.js webmail | [Client compatibility and display safety](specs/clients.md) |
| Terraform + Route 53 | [Provisioning, DNS and release workflow](operations/deployment.md) |
| GC/work-recovery jobs; CloudWatch/X-Ray | [Recovery](operations/recovery.md) and [monitoring](operations/monitoring.md) |
| Optional: LangGraph agent on AgentCore Runtime | [JMAP-only access, approvals, agent stores and phase gates](specs/agent.md) |

All mail backend Lambdas and terminal helpers use Go. Only the deferred upstream webmail uses its own Node.js server through Web Adapter, and the optional agent's runtime uses Python (its owner CLI is Go). SMTP2GO is the first outbound adapter; SES is receiving-only. The receiving region and pinned runtime/provider/client versions are deployment settings; webmail packaging is validated separately.

## Future extension points

An optional [mail agent](specs/agent.md) is built in stages, and its write scopes are granted only as its gates pass ([features](specs/agent.md#features)). It lives in this repository (`agent/`, `infra/agent/`) but deploys separately, and the mail system never depends on it. Its prerequisites are ordinary interfaces: OAuth app registration and per-method scopes, JMAP capability discovery/dispatch, single-writer index extensibility and content-free `EmailReceived` events through the outbox. The agent accesses mail only through JMAP, has no IAM access to mail storage and holds no authoritative mail data. Neither initial mail operation nor the agent prerequisite depends on deploying browser webmail.

## Design principles

1. Mail storage and JMAP remain complete without AI or a particular client.
2. Implement the standard and validate real clients early; client quirks do not redefine the server.
3. Preserve consistency and recovery evidence across asynchronous service boundaries.
4. Apply least privilege to every runtime, service, CI and recovery role from the first proof of concept.
5. Use serverless infrastructure where useful while budgeting connected compute and whole-index publication.
6. Automate infrastructure and checks; explicitly retain manual registrar delegation, account enrollment and relay signup.

Each topic spec's acceptance checks are its release gate. No integration is considered validated merely because it appears here.
