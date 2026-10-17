# Repository guidance

## Current state

- This repository currently contains design documents only.
- Application code, Terraform modules and automated test commands are planned.
- Do not claim a design gate has passed without implementation evidence.
- Start with `README.md`, then `docs/README.md` and `docs/architecture.md`.
- Read `docs/roadmap.md` before choosing implementation scope or priority.

## Task routing

Read the relevant owning document and its acceptance checks before making changes.

| Work | Canonical document |
| --- | --- |
| SES, aliases, MIME, SMTP, bounce handling | `docs/specs/mail-flow.md` |
| DynamoDB, state/history, counters, receipt markers, outbox | `docs/specs/storage-sync.md` |
| JMAP methods, limits, push, upload/download | `docs/specs/jmap.md` |
| OAuth, scopes, authorization, IAM, CI identity | `docs/specs/security.md` |
| SQLite, index publication/readers, text matching | `docs/specs/search.md` |
| aerc, helpers, browser hosting and display safety | `docs/specs/clients.md` |
| DNS, Terraform, CI/CD, shared test strategy | `docs/operations/deployment.md` |
| Retention, GC, rebuilds, owner recovery | `docs/operations/recovery.md` |
| Logs, metrics, alarms, tracing, budgets | `docs/operations/monitoring.md` |
| Stages, cutover prerequisites, release gates | `docs/roadmap.md` |
| Mail agent: LangGraph, AgentCore, agent tools and evals | `docs/specs/agent.md` |
| AWS architecture diagram and editing workflow | `docs/diagrams/README.md` |

## Scope and priorities

- Build and test with aerc plus CLI/protocol helpers first.
- Browser webmail is lower priority, deferred to Phase 4.
- Browser-only checks do not block core Phases 0–3 or CLI-based cutover.
- The later browser host is on-demand Lambda with Lambda Web Adapter.
- The mail agent (`agent/`, `infra/agent/`) is optional and starts after
  Phase 3 cutover; mail work never depends on it. Keep it a JMAP client:
  no agent IAM access to mail storage and no agent-specific server shortcuts.

## Implementation constraints

- Preserve the owning spec's invariants and failure behavior.
- Use least privilege for every created IAM role, including proofs of concept,
  CI, maintenance, recovery and later webmail; consult the security spec.
- Treat DynamoDB/S3 source data as authoritative; search is rebuildable.
- Preserve atomic state/history/outbox and replay/deletion protections.
- Never delete original S3 mail objects or their versions, including on Email
  destruction, replay or GC. Enable versioning, omit original-delete IAM grants
  and keep expiration/cleanup limited to disposable data; follow the recovery spec.
- Do not add backups in the current personal-use scope: no duplicate mail bucket,
  scheduled export jobs, AWS Backup plans or DynamoDB PITR/on-demand backups.
  Preserve original S3 versioning and document possible metadata loss.
- Never automatically resend a submission with unknown relay acceptance.
- Do not weaken shared protocol/authentication contracts for one client.
- Keep secrets, account IDs, real addresses and personal mail out of source,
  examples, fixtures and telemetry; use synthetic data and `example.com`.
- Keep client rendering safety and backend mail-processing policy in their
  respective owning specs.

## Documentation changes

- A requirement has one canonical home; link to it elsewhere.
- Update the relevant design, failure behavior and acceptance checks together.
- Keep architecture concise; detailed thresholds and contracts belong in specs.
- Keep priorities and release-gate routing in the roadmap.
- Keep `docs/diagrams/aws-mail-system.drawio.svg` current in the same change
  whenever AWS components, integrations, data flows, authorization boundaries,
  hosting or deployment stages change. Review it for related spec/IaC changes;
  if it needs no update, state why in the commit message (or the pull request
  for external contributions).
- The SVG contains its editable draw.io source. Update that source, regenerate
  the SVG with embedded diagram data, and visually inspect the result. Do not
  maintain a separate overview diagram or edit only the rendered SVG paths.
- Follow `docs/diagrams/README.md` for diagram scope and editing instructions.
- Use relative Markdown links and check local targets/anchors after moving files.
- Preserve old entry points when moving documentation referenced externally.
- Add a short decision record only for consequential new choices that need
  rationale beyond the owning spec; do not create a parallel requirement list.

## Validation and reporting

- Use trunk-based development: commit directly to `main` (no feature branches);
  run `make check` before pushing (needs Go, Terraform, golangci-lint and gitleaks;
  CI runs the same checks in `.github/workflows/checks.yml`).

- Follow the owning topic's acceptance checks and deployment validation tiers.
- Use the actual repository toolchain once it exists; do not invent runnable
  commands from examples of future tooling in the design.
- For documentation-only edits, check links, headings and preserved requirements.
- Report what changed, what was checked and any remaining validation limits.
