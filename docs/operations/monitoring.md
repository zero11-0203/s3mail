# Monitoring and telemetry

[Documentation index](../README.md) · [Architecture](../architecture.md) · [Roadmap](../roadmap.md)

## Scope and status

**Status: proposed; implementation and validation are pending.** Owns logs, metrics, alerts, tracing and cost visibility. Covers backend operations now and webmail when enabled in Phase 4.

## Required behavior and invariants

- Telemetry must not expose mail content, addresses or credentials.
- Alert delivery must work independently of this mailbox.
- Real-traffic alarms and maintenance completion checks are not continuous synthetic availability tests.
- Budget permanently retained original bytes/versions, disposable storage, reader refreshes, connected push duration and telemetry dimensions. Mailbox deletion does not reduce the original archive footprint.

## Design

Backend logging, metrics and tracing use **Amazon CloudWatch** and **AWS X-Ray**; no third-party observability service receives data. When webmail is deployed in Phase 4, its Lambda writes bounded-retention CloudWatch logs under the same privacy rule, without direct AWS mailbox data access or external analytics.

**Privacy rule for all telemetry:** logs, metric dimensions and span attributes contain identifiers (email id, thread id, mailbox id, JMAP method, relay message id, request and trace ids) but never message bodies, subjects, attachment names, email addresses or tokens. The [security acceptance checks](../specs/security.md#acceptance-checks) enforce this.

### Logging (CloudWatch Logs)

- Go code logs structured JSON with `log/slog`; Lambda's JSON log format and application log level are set in Terraform, so verbosity changes need no code change. The later Node.js webmail's application/adapter logs must satisfy the same privacy and retention checks before it is enabled.
- Every entry carries the Lambda request id, the X-Ray trace id, the component and the outcome; JMAP calls also log method name, latency and error type. The trace id is returned to clients in a response header so a failing request can be found from the client side.
- Each Lambda has an explicitly created log group (so Terraform owns retention) with **30-day retention**; access logs from API Gateway go to their own log group.
- Gateway/proxy logs record route templates and request IDs, never full download paths, filenames, query strings or Authorization headers. Disable SDK wire/body debug logs and sanitize SMTP/provider error strings before logging; raw errors can contain addresses or message content.
- Saved **Logs Insights** queries (in Terraform) cover the common investigations: errors by component, slowest JMAP methods, failed submissions, authorization failures. Per-method analysis is done here rather than with high-cardinality metrics.

### Metrics (CloudWatch)

- **Built-in metrics** cover most needs at no extra cost: Lambda errors, throttles and duration; API Gateway 4xx/5xx and latency; SQS age of oldest message and DLQ depth; DynamoDB throttles.
- **Custom metrics** use Embedded Metric Format (EMF), with no `PutMetricData` on the request path. Keep a small fixed set under `Mail`, without per-message/account dimensions: ingestion outcome, oldest outbox/dirty-index age, publication size/count, accepted/failed/unknown sends, bounces, quota use, push connections, auth failures, canary result, oldest derived-cleanup job. Review the metric count/cost when adding dimensions. Canary nonces stay in expiring control records, not metric dimensions.

### Alarms and dashboard

- Alarms cover any DLQ entry, ingestion/outbox backlog, JMAP failures, expired send leases/unknown acceptance, relay quota, index lag over six minutes, snapshot size/cold-search latency, auth spikes, missing work-recovery/GC completion, derived cleanup approaching seven days and cost budgets. Real-traffic alarms have the accepted no-traffic blind spot ([deployment policy](deployment.md#cicd)); they are not represented as an end-to-end availability monitor.
- Alarm notifications go through SNS to an **address outside this mail system** (and optionally a mobile push), so alerts still arrive when the mail system itself is broken.
- One CloudWatch dashboard shows the mail path end to end: inbound volume and lag, JMAP latency and errors, submissions and bounces, push connections, alarms.

### Tracing (X-Ray)

- **Active tracing** is enabled on every Lambda and on the API Gateway stage, so each request gets a trace from the edge through the handler.
- Instrumentation uses the **OpenTelemetry Go SDK** exporting to X-Ray (AWS Distro for OpenTelemetry), not the X-Ray SDK for Go, which entered maintenance mode in February 2026. Spans wrap DynamoDB, S3, SQS, Cognito JWKS fetches, SMTP submission and index downloads.
- Trace context propagates from instrumented ingestion through dispatcher/SQS work using supported trace headers/links. SES/SNS receipt delivery does not originate an application trace, and separate retries/batches may be linked traces rather than one continuous trace. Do not put mail headers/content into propagation attributes.
- **Sampling:** at personal volume, sample all API and ingestion requests; the push Lambda records one trace per connection without spans for its periodic state polls, to keep trace size and cost down. Usage should stay within the X-Ray free tier.

## Failure handling

Investigate DLQs, backlog, unknown relay acceptance and failed maintenance using the dashboard and content-free identifiers. Follow [mail-flow reconciliation](../specs/mail-flow.md#outbound-email) for uncertain sends and [recovery](recovery.md#failure-handling) for restoration. External SNS alarm delivery remains available during mail outages.

## Acceptance checks

- Capture logs, metrics and spans across normal and failing proxy/SDK/SMTP/webhook paths; assert the privacy rule.
- Exercise alarms and external notification delivery, dashboard links and missing work-recovery/GC completion.
- Verify pinned OpenTelemetry/ADOT instrumentation on `provided.al2023`, linked queue retries and bounded push traces.
- Measure [search publication/reader costs](../specs/search.md#index-pipeline), push duration and custom metric count against the configured budget.
