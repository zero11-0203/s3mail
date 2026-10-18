# Implementation roadmap

[Documentation index](README.md) · [Architecture](architecture.md)

**Status:** Planned. No phase below is claimed complete. Core Phases 0–3 use aerc; browser webmail is lower priority and starts in Phase 4. The optional [mail agent](#agent-phases) starts after Phase 3 cutover.

## Release gates

Architecture choices are made; configured implementations still need proof. Resolve a failed applicable gate before releasing that component. These links are the canonical acceptance checklists, rather than a second copy of their requirements.

| Gate | Required evidence lives in |
| --- | --- |
| Region, DNS/SES, binary/stream integration and pinned versions | [Deployment](operations/deployment.md#acceptance-checks), [JMAP](specs/jmap.md#acceptance-checks) |
| Alias, envelope, MIME disposition, relay TLS/Message-ID, quotas and webhooks | [Mail flow](specs/mail-flow.md#acceptance-checks) |
| Transactions, counters, state paging, history and outbox recovery | [Storage and sync](specs/storage-sync.md#acceptance-checks) |
| Protocol matrix, push, response/blob limits and safe download handling | [JMAP](specs/jmap.md#acceptance-checks) |
| Tokens, side effects, all IAM roles and CI data-access restrictions | [Security](specs/security.md#acceptance-checks) |
| Search completeness, snapshot publication, reader freshness, cost and retention | [Search](specs/search.md#acceptance-checks) |
| Initial aerc/OAuth slice, response loss and two CLI sessions | [Clients](specs/clients.md#acceptance-checks) |
| Permanent originals, derived cleanup, owner access and outbound-paused rebuild | [Recovery](operations/recovery.md#acceptance-checks) |
| Positive release acceptance, nonce canary and coordinated rollback | [Deployment](operations/deployment.md#acceptance-checks) |
| Mail-free telemetry, dashboard, budgets and tracing | [Monitoring](operations/monitoring.md#acceptance-checks) |
| Browser image, sessions, proxy/stream limits, display safety and CLI/browser state | [Phase 4 browser gate](specs/clients.md#browser-release-gate-phase-4) |
| Agent token path, read-only scope, cited answers, injection resistance and mail-free telemetry, then each later agent phase | [Agent](specs/agent.md#acceptance-checks) |

## Implementation phases

### Phase 0 — Prove the risky integrations

- Bootstrap the state/CI roles with the owner identity and start the local test harness. Use a pilot subdomain and synthetic mail for the deployed slice; keep primary MX routing until cutover gates pass.
- Select and record the AWS region and pinned Go runtime/provider/aerc/library versions. Deploy the Regional REST API streaming proof and common authorization contract. Webmail image/adapter selection and hosting are deferred to Phase 4.
- Provision the pilot SES identity/MX, receipt SNS/SQS path, Cognito user/scopes and basic durable mail/submission records. Verify envelope recipients, unknown-recipient behavior, relay TLS/Message-ID/authentication and webhook correlation (receiving and ingestion: [SES receiving evidence](evidence/phase0-ses-receiving.md)).
- Build the minimal `mail-auth` flow and connect aerc: login/refresh, session discovery, one message, a blob and one submission including response-loss behavior. Use the protocol helper for operations not exposed directly by the CLI. No webmail deployment is required for this slice (read path without submission: [aerc read slice evidence](evidence/phase0-aerc-slice.md)).
- Check actual binary upload and streaming response limits (measured with the Cognito authorizer and real tokens; see the [streaming spike evidence](evidence/phase0-streaming-spike.md)). Replace an incompatible client/adapter before expanding the backend; do not weaken the shared protocol/security contract to disguise a failed gate.

### Phase 1 — Durable mail transport and storage

- Complete Route 53 declarations/delegation with existing-record preservation, verified SES receiving identity and explicit alias declarations.
- Establish the versioned original bucket with permanent retention, no delete grants for relevant roles and no expiration rules; keep disposable data separate. Verify deletes cannot reach originals.
- Implement the guarded DynamoDB schema, counters/listings/threading, receipt/deletion markers, ordered changes and transactional outbox. Establish queue/DLQ policies, recovery sweeps and alarms.
- Implement the `smtp` and `fake` adapters, frozen send material, atomic quotas, attempt leases, cancellation, ambiguous-send handling and authenticated bounce ingestion.
- Implement verdict routing, MIME resource bounds and Quarantine recovery. Prove duplicate/deleted receipt replay and every commit/dispatch/handoff crash boundary.

### Phase 2 — Complete JMAP and search

- Finish every method in the conformance matrix, exact scope/side-effect checks, blob ownership/pins and consistent query/change pagination.
- Implement the complete-source indexer, coherent snapshot/CAS publication, generation registration, separate search Lambda and field-complete multilingual matching.
- Configure bounded search-version retention, deletion-aware refresh/purge and scale benchmarks. Build the sync model, MIME, authorization and search publication tests alongside implementation.
- Complete streamed API/body/blob responses, EventSource with token-aware reconnect and polling fallback. Continuously run the aerc replays, CLI acceptance and protocol suites; browser integration remains deferred.

### Phase 3 — Recovery and primary-mail cutover

- Seven-day derived-data GC that excludes originals ([deployed collector](operations/recovery.md#derived-data-collector)). The owner chose not to rehearse independent owner/identity recovery or an outbound-paused rebuild, nor to create a separate recovery role; the [runbook](operations/recovery.md#restore-runbook) stays documented and untested. Verify the accepted metadata-loss behavior without backups; do not add backup buckets, scheduled exports or DynamoDB PITR. Reconcile surviving deletion/attempt evidence before resuming use.
- Complete deployed negative/infrastructure checks, nonce-bound canary, owner positive acceptance, external deliverability and coordinated code/infrastructure rollback procedures.
- Verify the core Phases 0–3 gates in [release gates](roadmap.md#release-gates), including aerc/CLI acceptance, transport limits, bounded index costs and absence of CI data access. Move primary MX/use only after these pass. Browser hosting, browser HTML-safety tests and CLI/browser interoperability are Phase 4 gates, so they do not delay this CLI-based cutover.

### Phase 4 — Lower-priority browser webmail

- Once the core mail system is operational, select/pin the upstream Next.js client and Lambda Web Adapter versions. Validate stateless OAuth/PKCE/session handling, `/tmp` cache writes and cold starts using the actual Lambda container.
- Bootstrap scoped ECR/image-deploy permissions. Register the webmail in the main root (Cognito browser app client, authorizer allowlist, CORS origin), then build/push the pinned image and apply the separate `infra/webmail` root for its Lambda alias, gateway, domain and certificate ([webmail deployment](operations/deployment.md#webmail-deployment)). The Terraform and image build exist; none of it has been applied or validated.
- Complete browser acceptance from [clients](specs/clients.md#design): login/refresh/logout across instances, cookie/cache safety, HTML/sandbox/privacy protections, attachments, streaming/proxy payload limits and synchronization with aerc. Activate browser access only after this component's checks pass.
- Add the browser fixtures, image scans, budget/latency alarms and release/rollback manifest entries. aerc remains the independent CLI test and access path.

### Phase 5 — Measured follow-ups

- Shard the search index or move it to a managed database only when measured size/latency thresholds justify it.
- Add WebSocket push or a larger upload path only when needed; keep limits/discovery/tests synchronized.
- Continue security/version updates, recovery rehearsals on relevant storage changes and per-release client acceptance. The extension points in [future extensions](architecture.md#future-extension-points) already exist as ordinary capabilities/events; no agent feature is required for mail operation.

## Agent phases

The [mail agent](specs/agent.md) is optional and lower priority. It begins after the mail system's Phase 3 cutover requirements pass; neither that prerequisite nor primary-mail operation depends on the browser deployment, and the mail system never depends on the agent. Each phase adds one class of side effect and the OAuth scope it needs, and starts only after the previous phase's [gate](specs/agent.md#acceptance-checks) passes. A0–A3 are independent of Phase 4; only webmail chat in A4 needs it.

**Owner decision (2026-10-18): build A1–A4 ahead of the cutover.** The owner chose to implement the later agent phases before the Phase 3 cutover, as with A0. The code for A1–A4 is built and tested together, but the order of side effects is kept: A1 is deployed with `mail.read` only, and the agent's Cognito client gets `mail.write` and then `mail.submit` only after the A1 and then the A2 gate passes ([granting a scope](operations/deployment.md#granting-agent-scopes)). The owner later waived the remaining A1 checks (2026-10-18), so `mail.write` is granted when the owner chooses, and enabled semantic search without an evaluation set; attachment text stays off until its evaluation passes. The cutover requirements and mail operation still never depend on the agent.

### A0 — MVP: verify the path

- Prerequisite: register the agent's Cognito app client with `s3mail/mail.read` only and add it to the authorizer allowlist in `infra/main`.
- Deploy `infra/agent` with the AgentCore runtime, Memory and Identity credential provider. Ship the `agent` CLI with `login` and `ask`, the read-only JMAP tools, citation checks and content-free tracing.
- Build the synthetic mailbox, question and injection sets and the telemetry scan. Rehearse the kill switch. No feature has side effects.
- The code, image, Terraform and offline tests exist and were deployed on 2026-10-18 ahead of the cutover ([agent deployment](operations/deployment.md#agent-deployment)); the evaluation sets and the remaining gate checks are open.

### A1 — Triage and digest

- Add the EventBridge rule on `EmailReceived`, the queue, DLQ and trigger Lambda, the agent table, triage mode and the daily digest. The scope stays `mail.read`.
- Implemented with offline tests and the triage evaluation, and deployed (2026-10-18); the owner waived the remaining [A1 gate](specs/agent.md#a1-gate-triage-and-digest) checks.

### A2 — Drafts

- Grant `mail.write`. Add reply drafts restricted to the Drafts mailbox, the digest delivered as an email and owner-stated preferences in long-term memory.
- Implemented with offline tests and the draft rubric suite (mean 4.99 of 5 over 30 threads, 2026-10-18); deployed in the same image, but `mail.write` is not granted yet, so the write modes stop with `scope_not_granted`. The owner spot check of the drafts is open.

### A3 — Approved actions

- Grant `mail.submit`. Add proposals, approval interrupts, the hash-checked executor and the send-once marker for organising actions and sending reviewed drafts, plus reply reminders.
- Implemented with offline tests (the approval scenarios run against a stateful fake server); `mail.submit` is not granted.

### A4 — Measured extensions

- Choose from what A1–A3 show is used: semantic search through `docs_vec` and a custom JMAP capability, attachment text, the tools as an MCP server behind AgentCore Gateway, and webmail chat after Phase 4. Each ships with its own evaluation gate.
- The owner chose attachment text and semantic search (embeddings through OpenRouter). Both agent tools are implemented behind `AGENT_FEATURES`. Semantic search (embeddings in the indexer, the `SemanticEmail/query` method and the agent's `semantic` tool) was deployed on 2026-10-18 at the owner's request, without an evaluation set; it sends indexed mail text to OpenRouter. Attachment text stays off. The MCP server and webmail chat are deferred: nothing outside the CLI uses the tools yet, and webmail chat needs Phase 4.
