# Documentation index

[Project README](../README.md) · [Architecture](architecture.md) · [Roadmap](roadmap.md)

**Current state:** The design is implemented in part: the Go backend and helpers, the Terraform roots and modules, and the optional Python mail agent, with tests run by `make check`. Parts are deployed for validation. No release gate counts as passed unless its owning spec records the evidence; the [roadmap](roadmap.md) tracks phases.

Start with the [architecture](architecture.md) for the five-minute overview, then the [roadmap](roadmap.md) for implementation order and release gates. Coding agents should also read [AGENTS.md](../AGENTS.md).

The [AWS architecture diagram](diagrams/aws-mail-system.drawio.svg) is embedded in the overview and editable in draw.io. Architecture changes must update it in the same change; see the [diagram maintenance guide](diagrams/README.md).

## Topic specifications

Each spec owns its requirements, failure behavior and acceptance checks. Read the relevant topic rather than the entire design.

| Topic | Document |
| --- | --- |
| Receipt, aliases, MIME limits, outbound relay and bounces | [Mail flow](specs/mail-flow.md) |
| Account model, DynamoDB transactions, sync state and outbox | [Storage and sync](specs/storage-sync.md) |
| Protocol coverage, push, limits and blobs | [JMAP](specs/jmap.md) |
| Implemented JMAP methods, limits and tests | [JMAP conformance matrix](specs/jmap-conformance.md) |
| Cognito, endpoint/method authorization, IAM and CI trust | [Security](specs/security.md) |
| SQLite indexing, multilingual matching, retention and cost | [Search](specs/search.md) |
| aerc, OAuth helpers, deferred webmail and display safety | [Clients](specs/clients.md) |
| Later LangGraph/AgentCore agent, its tools, approvals and phase gates | [Mail agent](specs/agent.md) |

## Evidence

Measured Phase 0 results and implementation reviews; requirements stay in the owning specs.

- [REST API response streaming spike](evidence/phase0-streaming-spike.md)
- [SES receiving](evidence/phase0-ses-receiving.md)
- [aerc read slice](evidence/phase0-aerc-slice.md)
- [JMAP RFC conformance review](evidence/jmap-rfc-review.md)
- [Mail agent design and implementation review](evidence/agent-review.md)
- [Mail agent implementation review — 2026-10-19](evidence/agent-implementation-review-2026-10-19.md)
- [Review of the agent fixes in e69137d](evidence/agent-fix-review-e69137d.md)
- [Review of the agent fixes in ae72106](evidence/agent-fix-review-ae72106.md)
- [Review of the agent fixes in 48a5b2e](evidence/agent-fix-review-48a5b2e.md)
- [Review of the agent fixes in e14c353](evidence/agent-fix-review-e14c353.md)
- [Phase 3 gate audit](evidence/phase3-gate-audit.md)

## Operations

| Task | Document |
| --- | --- |
| DNS, Terraform, release workflow and shared testing strategy | [Deployment](operations/deployment.md) |
| Permanent originals, mailbox deletion and rebuilds | [Recovery](operations/recovery.md) |
| Deployed IAM roles and resource-policy grants | [Permission inventory](operations/permissions.md) |
| Logs, metrics, alarms, tracing and budgets | [Monitoring](operations/monitoring.md) |

A requirement has one canonical home. Other documents should link to it. Update the owning spec and its acceptance checks together when behavior changes; use the roadmap for stage/priority changes. Add a short decision record under `docs/decisions/` only when a new consequential choice needs rationale beyond its owning spec.
