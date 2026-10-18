# Monitoring and telemetry

[Documentation index](../README.md) · [Architecture](../architecture.md) · [Roadmap](../roadmap.md)

## Scope and status

**Status: partly implemented; see [current implementation](#current-implementation).** Owns logs, metrics, alerts, tracing and cost visibility. Covers backend operations now and webmail when enabled in Phase 4.

## Required behavior and invariants

- Telemetry must not expose mail content, addresses or credentials.
- Operational state is visible without this mailbox, on the dashboard and in X-Ray. Alarms and an external notification channel are optional; if a channel is added, its delivery must not depend on this mail system.
- Real-traffic alarms and maintenance completion checks, where added, are not continuous synthetic availability tests.
- Budget permanently retained original bytes/versions, disposable storage, reader refreshes, connected push duration and telemetry dimensions. Mailbox deletion does not reduce the original archive footprint.

## Design

Backend logging, metrics and tracing use **Amazon CloudWatch** and **AWS X-Ray**; no third-party observability service receives data. When webmail is deployed in Phase 4, its Lambda writes bounded-retention CloudWatch logs under the same privacy rule, without direct AWS mailbox data access or external analytics.

**Privacy rule for all telemetry:** logs, metric dimensions and span attributes contain identifiers (email id, thread id, mailbox id, JMAP method, relay message id, request and trace ids) but never message bodies, subjects, attachment names, email addresses or tokens. The [security acceptance checks](../specs/security.md#acceptance-checks) enforce this.

### Logging (CloudWatch Logs)

- Go code logs structured JSON with `log/slog`; Lambda's JSON log format and application log level are set in Terraform, so verbosity changes need no code change. The later Node.js webmail's application/adapter logs must satisfy the same privacy and retention checks before it is enabled. Its server writes its own JSON lines at `warn` (`LOG_FORMAT`/`LOG_LEVEL`), so its function uses Lambda's text log format with 30-day retention; its separate root can add a `Component=webmail` budget.
- Every entry carries the Lambda request id, the X-Ray trace id, the component and the outcome; JMAP calls also log method name, latency and error type. The trace id is returned to clients in a response header so a failing request can be found from the client side.
- Each Lambda has an explicitly created log group (so Terraform owns retention) with **30-day retention**; access logs from API Gateway go to their own log group.
- Gateway/proxy logs record route templates and request IDs, never full download paths, filenames, query strings or Authorization headers. Disable SDK wire/body debug logs and sanitize SMTP/provider error strings before logging; raw errors can contain addresses or message content.
- Saved **Logs Insights** queries (in Terraform) cover the common investigations: errors by component, slowest JMAP methods, failed submissions, authorization failures. Per-method analysis is done here rather than with high-cardinality metrics.

### Metrics (CloudWatch)

- **Built-in metrics** cover most needs at no extra cost: Lambda errors, throttles and duration; API Gateway 4xx/5xx and latency; SQS age of oldest message and DLQ depth; DynamoDB throttles.
- **Custom metrics**, if added, use Embedded Metric Format (EMF), with no `PutMetricData` on the request path. Each one is billed, so none exist now. Keep a small fixed set under `Mail`, without per-message/account dimensions: ingestion outcome, oldest outbox/dirty-index age, publication size/count, accepted/failed/unknown sends, bounces, quota use, push connections, auth failures, canary result, oldest derived-cleanup job. Review the metric count/cost when adding dimensions. Canary nonces stay in expiring control records, not metric dimensions.

### Alarms and dashboard

- The owner chose **no alarms** for now, because each is billed. Problems are found on the dashboard, in X-Ray and with `mail-admin submissions`.
- If alarms are added later, they cover any DLQ entry, ingestion/outbox backlog, JMAP failures, expired send leases/unknown acceptance, relay quota, index lag over six minutes, snapshot size/cold-search latency, auth spikes, missing work-recovery/GC completion, derived cleanup approaching seven days and cost budgets. Real-traffic alarms have the accepted no-traffic blind spot ([deployment policy](deployment.md#cicd)); they are not an end-to-end availability monitor. Notifications go through SNS to an **address outside this mail system** (and optionally a mobile push), so alerts still arrive when the mail system itself is broken.
- One CloudWatch dashboard of built-in metrics shows the mail path end to end, within the free dashboard allowance (50 metrics).
- The optional [mail agent](../specs/agent.md#deployment-and-operations) is not on this dashboard: the mail path never depends on it. Its problems show as messages in its triage DLQ (`infra/agent` output `triage_dlq_url`), trigger Lambda errors and content-free error codes in the runtime logs. A failed `EmailReceived` publish keeps the outbox entry and shows as dispatcher errors here.

### Tracing (X-Ray)

- **Active tracing** is enabled on every Lambda and on the API Gateway stage, so each request gets a trace from the edge through the handler.
- Instrumentation uses the **OpenTelemetry Go SDK**, not the X-Ray SDK for Go, which entered maintenance mode in February 2026. Spans are sent to the X-Ray daemon that Lambda runs for active tracing, as OTLP over UDP, the format the AWS distros' own Lambda exporters use; no collector layer or Transaction Search is needed. Spans wrap every AWS SDK call (DynamoDB, S3, SQS, SSM, Secrets Manager, Lambda), Cognito JWKS fetches and SMTP submission, so the X-Ray trace map draws the architecture from real traffic.
- Trace context propagates through the AWS SDK's trace header (SQS messages, Lambda invocations) and through the outbox: each `OUTBOX#` record carries the committing request's X-Ray header, and the dispatcher continues that trace when it hands the work on, linked to its own invocation. SES/SNS receipt delivery does not originate an application trace, and separate retries/batches may be linked traces rather than one continuous trace. Do not put mail headers/content into propagation attributes.
- **Sampling:** at personal volume, sample all API and ingestion requests; the push Lambda records one trace per connection without spans for its periodic state polls, to keep trace size and cost down. Usage should stay within the X-Ray free tier.

## Current implementation

What is deployed (`infra/modules/ops`, the Lambda modules and `go/internal/send`) and where it is narrower than the design above:

- **Dashboard** (`terraform output dashboard_url`): function errors and throttles, the depth of every queue including the DLQs, work-queue message age, API requests, errors and latency. No alarms or custom metrics.
- **Tracing** (`go/internal/telemetry`): active X-Ray tracing on every Lambda and on the API stage; each invocation has a `handler` span nested in the function's segment, with client spans for the AWS SDK calls, the JWKS fetch and an `SMTP submit` span naming the relay host, stage, outcome, recipient count and relay message id. The push function records only its connection span. Outbox records carry the trace header (`trace`), so a send is traced from `EmailSubmission/set` through the dispatcher, the submission queue and the worker to the relay. Tests check that span attributes hold no addresses, item values or message bodies.
- **Not yet:** alarms and a notification channel (not wanted for now), cost budgets, custom metrics, saved Logs Insights queries and the trace id in responses.

## Failure handling

Investigate DLQs, backlog, unknown relay acceptance and failed maintenance using the dashboard, X-Ray traces and content-free identifiers. Follow [mail-flow reconciliation](../specs/mail-flow.md#outbound-email) for uncertain sends and [recovery](recovery.md#failure-handling) for restoration. Without alarms, problems are seen only when the owner looks: check the dashboard's DLQ depth and `mail-admin submissions` after sending.

## Acceptance checks

- Capture logs, metrics and spans across normal and failing proxy/SDK/SMTP/webhook paths; assert the privacy rule.
- Exercise alarms and external notification delivery if configured, dashboard links and missing work-recovery/GC completion.
- Verify the pinned OpenTelemetry instrumentation on `provided.al2023` reaches X-Ray, linked queue retries and bounded push traces.
- Measure [search publication/reader costs](../specs/search.md#index-pipeline), push duration and custom metric count against the configured budget.
