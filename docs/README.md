# Documentation index

[Project README](../README.md) · [Architecture](architecture.md) · [Deploy guide](guides/deploy.md)

**Current state:** the Go backend and helpers, the Terraform roots and modules, and the optional Python mail agent are implemented, with offline tests run by `make check`. A reference deployment runs them for validation. No acceptance check counts as passed unless its owning spec records the evidence; see the [limitations](../README.md#limitations).

Start with the [architecture](architecture.md) for the five-minute overview. To run your own copy, follow the [guides](#guides). Contributors should read [CONTRIBUTING.md](../CONTRIBUTING.md), and coding agents also [AGENTS.md](../AGENTS.md).

The [AWS architecture diagram](diagrams/aws-mail-system.drawio.svg) is embedded in the overview and editable in draw.io. Architecture changes must update it in the same change; see the [diagram maintenance guide](diagrams/README.md).

## Guides

For people who deploy and use s3mail.

| Guide | Contents |
| --- | --- |
| [Deploy](guides/deploy.md) | Prerequisites, step-by-step deployment into your own AWS account, updates and troubleshooting |
| [Usage](guides/usage.md) | Signing in, aerc setup, addresses, sending limits, failed sends and routine checks |
| [Costs](guides/costs.md) | Estimated monthly AWS cost for idle, light, moderate and heavy use |

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
| Optional LangGraph/AgentCore agent, its tools, approvals and stage gates | [Mail agent](specs/agent.md) |

Each spec's **Acceptance checks** section is the release gate for its topic.

## Operations

| Task | Document |
| --- | --- |
| DNS, Terraform, release workflow and shared testing strategy | [Deployment](operations/deployment.md) |
| Permanent originals, mailbox deletion and rebuilds | [Recovery](operations/recovery.md) |
| Deployed IAM roles and resource-policy grants | [Permission inventory](operations/permissions.md) |
| Logs, metrics, alarms, tracing and budgets | [Monitoring](operations/monitoring.md) |

## Terms used in the specs

- **Core system:** everything needed to receive, store, serve, search and send mail with aerc and the CLI helpers. Browser webmail and the mail agent are not part of it.
- **Primary-mail cutover:** the point where an operator starts using a deployment for everyday mail. It should wait until the core acceptance checks pass.
- **Deferred browser webmail:** the optional browser client hosted on Lambda. Its Terraform and image build exist, but it has not been validated, and its checks never block the core system.
- **Agent stages A0–A4:** the mail agent's steps, each adding one class of side effect and the OAuth scope it needs; defined in the [agent spec](specs/agent.md#features).

A requirement has one canonical home. Other documents should link to it. Update the owning spec and its acceptance checks together when behavior changes. Add a short decision record under `docs/decisions/` only when a new consequential choice needs rationale beyond its owning spec.
