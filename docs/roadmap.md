# Implementation roadmap

[Documentation index](README.md) · [Architecture](architecture.md)

**Status:** Planned. No phase below is claimed complete. Core Phases 0–3 use aerc; browser webmail is lower priority and starts in Phase 4.

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
| Mail-free telemetry, external alerts, budgets and tracing | [Monitoring](operations/monitoring.md#acceptance-checks) |
| Browser image, sessions, proxy/stream limits, display safety and CLI/browser state | [Phase 4 browser gate](specs/clients.md#browser-release-gate-phase-4) |

## Implementation phases

### Phase 0 — Prove the risky integrations

- Bootstrap the state/CI roles with the owner identity and start the local test harness. Use a pilot subdomain and synthetic mail for the deployed slice; keep primary MX routing until cutover gates pass.
- Select and record the AWS region and pinned Go runtime/provider/aerc/library versions. Deploy the Regional REST API streaming proof and common authorization contract. Webmail image/adapter selection and hosting are deferred to Phase 4.
- Provision the pilot SES identity/MX, receipt SNS/SQS path, Cognito user/scopes and basic durable mail/submission records. Verify envelope recipients, unknown-recipient behavior, relay TLS/Message-ID/authentication and webhook correlation.
- Build the minimal `mail-auth` flow and connect aerc: login/refresh, session discovery, one message, a blob and one submission including response-loss behavior. Use the protocol helper for operations not exposed directly by the CLI. No webmail deployment is required for this slice.
- Check actual binary upload and streaming response limits ([streaming spike evidence](evidence/phase0-streaming-spike.md); authorizer and JWT-size checks remain). Replace an incompatible client/adapter before expanding the backend; do not weaken the shared protocol/security contract to disguise a failed gate.

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

- Rehearse independent owner/identity recovery, seven-day derived-data GC that excludes originals and an outbound-paused rebuild using surviving metadata/originals. Verify the accepted metadata-loss behavior without backups; do not add backup buckets, scheduled exports or DynamoDB PITR. Reconcile surviving deletion/attempt evidence before resuming use.
- Complete deployed negative/infrastructure checks, nonce-bound canary, owner positive acceptance, external deliverability and coordinated code/infrastructure rollback procedures.
- Verify the core Phases 0–3 gates in [release gates](roadmap.md#release-gates), including aerc/CLI acceptance, transport limits, bounded index costs and absence of CI data access. Move primary MX/use only after these pass. Browser hosting, browser HTML-safety tests and CLI/browser interoperability are Phase 4 gates, so they do not delay this CLI-based cutover.

### Phase 4 — Lower-priority browser webmail

- Once the core mail system is operational, select/pin the upstream Next.js client and Lambda Web Adapter versions. Validate stateless OAuth/PKCE/session handling, `/tmp` cache writes and cold starts using the actual Lambda container.
- Bootstrap scoped ECR/image-deploy permissions and enable the optional Terraform webmail module. Build/push the pinned image, deploy its Lambda alias and separate gateway, and provision the domain/certificate, Cognito browser app client, session secrets and CORS registration.
- Complete browser acceptance from [clients](specs/clients.md#design): login/refresh/logout across instances, cookie/cache safety, HTML/sandbox/privacy protections, attachments, streaming/proxy payload limits and synchronization with aerc. Activate browser access only after this component's checks pass.
- Add the browser fixtures, image scans, budget/latency alarms and release/rollback manifest entries. aerc remains the independent CLI test and access path.

### Phase 5 — Measured follow-ups

- Shard the search index or move it to a managed database only when measured size/latency thresholds justify it.
- Add WebSocket push or a larger upload path only when needed; keep limits/discovery/tests synchronized.
- Continue security/version updates, recovery rehearsals on relevant storage changes and per-release client acceptance. The extension points in [future extensions](architecture.md#future-extension-points) already exist as ordinary capabilities/events; no agent feature is required for mail operation.

The separate AI agent project can begin after the mail system's Phase 3 cutover requirements pass; its design is reviewed independently. Neither that prerequisite nor primary-mail operation depends on completing the lower-priority browser deployment.
