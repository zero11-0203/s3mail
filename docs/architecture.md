# Mail system architecture

[Documentation index](README.md) · [Roadmap](roadmap.md)

**Status:** Proposed architecture, implemented in part and deployed for validation; release gates are pending (see the [roadmap](roadmap.md)).  

## Overview

Build a personally controlled email system on AWS: SES receives mail, a pluggable SMTP relay sends it, S3 permanently retains original messages, DynamoDB owns metadata and synchronization state, and a Go Lambda API serves JMAP. Cognito supplies the shared OAuth identity; Terraform manages infrastructure and DNS. The backend works independently of any client or AI feature.

**One owner, one JMAP account, many addresses, one unified inbox.** Aliases select receiving and sending identities rather than separate logins. aerc is the first client for development, testing and initial production use. Browser webmail has lower priority and is deferred to Phase 4, using an on-demand Lambda container with AWS Lambda Web Adapter. Phases 0–3 and primary-mail cutover do not depend on browser deployment.

This page explains the system. The [topic specifications](README.md#topic-specifications) own detailed requirements and acceptance checks; [operations guides](README.md#operations) own deployment and recovery procedures.

## Scope

The initial system receives and sends custom-domain mail, supports multiple aliases with shared message state, and implements the required JMAP Core, Mail and Submission behavior. Existing OAuth-capable clients provide the UI. Account-scoped authorization, modest operational overhead and repeatable infrastructure are core goals.

Initial non-goals are multi-user hosting, a custom client or aerc fork, IMAP/SMTP submission compatibility, password/Basic/app-password authentication on mail endpoints, CalDAV/CardDAV and AI features. An IMAP gateway is a possible later extension.

As a public portfolio project, the interoperable JMAP server is deliberately built in Go. Go covers the backend and terminal helpers; AWS provides managed transport, storage and compute; Terraform demonstrates repeatable provisioning. Tests, CI, a readable README and short demos are part of delivery. The repository must contain no secrets, account IDs, real addresses or personal mail: examples use `example.com`, and private Terraform values stay gitignored. Python, LangGraph, AgentCore and an external model API (OpenRouter) are used only by the later, separately deployed [mail agent](specs/agent.md).

## Components and data flow

![AWS mail-system architecture: CLI-first mail paths, deferred Phase 4 webmail and the later mail agent](diagrams/aws-mail-system.drawio.svg)

[Open the editable AWS diagram](diagrams/aws-mail-system.drawio.svg) · [Editing and maintenance](diagrams/README.md)

This is a logical diagram. SES writes raw MIME and publishes the **S3 action receipt notification**, preserving envelope recipients and verdicts through SNS/SQS. Bare S3 notifications are insufficient. The domain stays at its registrar, with authoritative DNS delegated to Route 53. Lambdas use public AWS service endpoints without a VPC/NAT gateway; producer/account restrictions protect service integrations.

Incoming mail is durably parsed and committed with state/history/outbox work. JMAP clients read and mutate that same authoritative state. A dispatcher delivers committed work to submission and indexing queues. Submission workers retain attempt evidence across failures; uncertain relay acceptance requires owner reconciliation instead of automatic resend.

Original mail lives in a dedicated versioned S3 bucket whose relevant roles have no delete permissions. Deleting an Email removes active mailbox/search access while retaining its original; disposable uploads and derived data live separately. The [retention policy](operations/recovery.md#original-mail-retention) defines the boundary. No additional backups or DynamoDB PITR are configured for the current personal-use scope; loss of mailbox metadata may be permanent.

A single indexer publishes coherent SQLite FTS5 snapshots in S3; a separate search Lambda reads registered versions. Search is rebuildable and does not determine mailbox listing or synchronization correctness. Shared authorization covers all public JMAP routes, including streamed downloads and EventSource. The later webmail uses a separate gateway and its own browser session/login handling.

## Component responsibilities

| Component | Responsibility / detailed contract |
| --- | --- |
| SES → SNS/SQS → Go ingestion | [Envelope routing, aliases, MIME bounds and disposition](specs/mail-flow.md) |
| S3 + DynamoDB | [Content references, account model, transactions, counters and sync history](specs/storage-sync.md) |
| REST API Gateway + Go Lambdas | [JMAP methods, limits, push and blob streaming](specs/jmap.md) |
| Cognito + IAM | [User authorization and least privilege for every created role](specs/security.md) |
| Outbox + submission worker + SMTP2GO adapter | [Durable delivery, quotas and bounce correlation](specs/mail-flow.md#outbound-email) |
| SQLite FTS5 on S3 + indexer/search Lambdas | [Multilingual search, consistent publication and bounded version retention](specs/search.md) |
| aerc + mail-auth; later Next.js webmail | [Client compatibility and display safety](specs/clients.md) |
| Terraform + Route 53 | [Provisioning, DNS and release workflow](operations/deployment.md) |
| GC/work-recovery jobs; CloudWatch/X-Ray | [Recovery](operations/recovery.md) and [monitoring](operations/monitoring.md) |
| Later: LangGraph agent on AgentCore Runtime | [JMAP-only access, approvals, agent stores and phase gates](specs/agent.md) |

All mail backend Lambdas and terminal helpers use Go. Only the later upstream webmail uses its own Node.js server through Web Adapter, and the later agent's runtime uses Python (its owner CLI is Go). SMTP2GO is the planned first outbound adapter; SES is receiving-only. Phase 0 selects and verifies the receiving region and pinned runtime/provider/client versions. Phase 4 separately validates webmail packaging.

## Future extension points

An optional [mail agent](specs/agent.md) was planned to follow the Phase 3 cutover; the owner chose to build it ahead of the cutover, granting its write scopes only as its gates pass ([roadmap](roadmap.md#agent-phases)). It lives in this repository (`agent/`, `infra/agent/`) but deploys separately, and the mail system never depends on it. Its prerequisites are ordinary interfaces: OAuth app registration and per-method scopes, JMAP capability discovery/dispatch, single-writer index extensibility and content-free `EmailReceived` events through the outbox. The agent accesses mail only through JMAP, has no IAM access to mail storage and holds no authoritative mail data. Neither initial mail operation nor the agent prerequisite depends on deploying browser webmail.

## Design principles

1. Mail storage and JMAP remain complete without AI or a particular client.
2. Implement the standard and validate real clients early; client quirks do not redefine the server.
3. Preserve consistency and recovery evidence across asynchronous service boundaries.
4. Apply least privilege to every runtime, service, CI and recovery role from the first proof of concept.
5. Use serverless infrastructure where useful while budgeting connected compute and whole-index publication.
6. Automate infrastructure and checks; explicitly retain manual registrar delegation, account enrollment and relay signup.

Implementation order and release evidence live in the [roadmap](roadmap.md). No integration is considered validated merely because it appears here.
