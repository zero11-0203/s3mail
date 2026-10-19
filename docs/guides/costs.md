# Estimated monthly costs

[Documentation index](../README.md) · [Deploy guide](deploy.md) · [Usage guide](usage.md)

**Status:** an estimate, not a measurement. Prices are for `us-east-1` in October 2026: SES, API Gateway, DynamoDB, S3, CloudWatch Logs, Secrets Manager and X-Ray were checked against the AWS Price List API, and Lambda, SQS, Route 53 and Cognito use AWS's published rates. Usage figures are modeled from the code's settings, not from a bill. It has not yet been checked against a measured month. Confirm with the [AWS Pricing Calculator](https://calculator.aws/) for your region.

## Summary

| Profile | Mail per month | Clients | AWS cost per month |
| --- | --- | --- | --- |
| Idle (deployed, nothing happening) | 0 | none | **about $2.00** |
| Light | 1,500 in, 300 out | aerc 8 hours a day | **about $2.60** |
| Moderate | 5,000 in, 1,000 out | aerc always on, one computer | **about $4.30** |
| Heavy | 15,000 in, 3,000 out | aerc always on, two computers | **about $14.40** |

Not included:

- **Domain registration**, typically $10–20 a year at a registrar.
- **SMTP2GO above its free plan.** The free plan covers 1,000 messages a month and 200 a day, so it covers the light profile and just covers the moderate one. The heavy profile needs a paid plan; check [SMTP2GO pricing](https://www.smtp2go.com/pricing/).
- **Tax.**
- **Free-tier credits.** AWS accounts created after 15 July 2025 can receive sign-up credits for their first months, which can cover these costs for a while.

The figures use only the **always-free** allowances that every account keeps:

- Lambda: 1 million requests and 400,000 GB-seconds a month.
- SQS: 1 million requests.
- DynamoDB: 25 GB of storage.
- CloudWatch: 5 GB of logs and 3 dashboards.
- X-Ray: 100,000 traces.
- Cognito: 10,000 monthly active users.

The figures do not use the 12-month offers, such as API Gateway and S3, which newer accounts do not get.

## Fixed costs

These are paid even when no mail arrives.

| Item | Why it exists | Per month |
| --- | --- | --- |
| Route 53 hosted zone | DNS for the domain | $0.50 |
| Secrets Manager, 2 secrets | Relay SMTP credentials and webhook secret, at $0.40 each | $0.80 |
| SQS polling | Lambda long-polls 4 queues (ingest, index, submit, delivery) even when empty, about 650,000 requests per queue. That is about 2.6 million requests, less the 1 million free, at $0.40 per million. | ~$0.65 |
| Scheduled jobs | Work recovery every minute, a sweep every 5 minutes and daily GC: about 52,000 small Lambda runs, within the free tier | ~$0.01 |
| CloudWatch dashboard, logs, X-Ray | Within the free allowances while idle | $0.00 |
| Cognito, SSM parameter, EventBridge rules, SSE-S3 encryption | No charge at this scale | $0.00 |
| **Total** | | **about $2.00** |

## What usage costs

| Driver | Price | Notes |
| --- | --- | --- |
| Receiving mail (SES) | $0.10 per 1,000 messages plus $0.09 per 1,000 chunks of 256 KB | About $0.19 per 1,000 typical messages. A 1 MB message counts as 4 chunks. |
| Mail storage (S3 Standard) | $0.023 per GB-month | Originals are never deleted, so this only grows: about $0.28 a year per GB received |
| Search index versions (S3) | $0.023 per GB-month | Each publication uploads the whole index. The current one and three previous versions are kept, and older versions expire after a day. A large index (300 MB) published often can store 30 GB or more of short-lived versions. |
| Connected clients (Lambda push) | $0.0000133334 per GB-second (arm64) | Each open aerc holds a 128 MB push function: **about $4.30 a month per client connected around the clock**, once the 400,000 GB-second free allowance is used. About 1.2 such clients fit in the free allowance. |
| JMAP requests (API Gateway REST) | $3.50 per million | Light use is tens of thousands of requests a month. Streamed responses are counted here as ordinary requests; check API Gateway pricing for any streaming charges. |
| Mailbox reads and writes (DynamoDB on demand) | $0.625 per million write units, $0.125 per million read units | About 40 write units per received message (transactional). Push polls the state record every 3 seconds per connected client. |
| Tracing (X-Ray) | $5 per million traces beyond 100,000 | The minute-by-minute sweeps alone use about 52,000 traces, so moderate use goes slightly over |
| Logs (CloudWatch) | $0.50 per GB ingested beyond 5 GB | Personal use stays within the free 5 GB |
| Sending | SMTP2GO plan | AWS charges for sending are only the Lambda runs and requests above |

## Scenario details

Assumptions:

- **Light:** messages average 75 KB, 1 GB archive.
- **Moderate:** messages average 100 KB, 1.2 chunks each, 5 GB archive, 60 MB index.
- **Heavy:** messages average 150 KB, 1.3 chunks each, 20 GB archive, 50,000-message (300 MB) index.

| Item | Light | Moderate | Heavy |
| --- | --- | --- | --- |
| Fixed costs | $1.96 | $1.96 | $1.96 |
| SES receiving | $0.29 | $1.04 | $3.26 |
| Lambda: ~120k / 364k / 748k GB-seconds | $0.00 | $0.00 | $4.64 |
| API Gateway: ~25k / 80k / 250k requests | $0.09 | $0.28 | $0.88 |
| DynamoDB | $0.12 | $0.35 | $0.95 |
| S3: originals, index versions, requests | $0.10 | $0.32 | $1.40 |
| X-Ray | $0.00 | $0.25 | $1.25 |
| Route 53 queries, Secrets Manager calls | $0.03 | $0.05 | $0.07 |
| **Total (rounded)** | **$2.60** | **$4.30** | **$14.40** |

## Optional components

| Component | Extra per month | Notes |
| --- | --- | --- |
| Browser webmail (deferred) | about $0.35–2 | Up to 10 retained images in ECR at $0.10 per GB (about $0.30). The webmail Lambda runs only while used, so it is often within the free tier. The webmail polls JMAP instead of using push. |
| Semantic search | about $0.40 plus embedding calls | One more Secrets Manager secret for the OpenRouter key; embedding calls are billed by OpenRouter per token. Off by default. |
| Mail agent | about $1.80 plus usage | A KMS key ($1.00) and two secrets ($0.80) while deployed; AgentCore Runtime and Memory are billed per use, and model calls are billed by OpenRouter. Not estimated here; set a credit limit on the OpenRouter key. Off by default. |
| CloudWatch alarms | $0.10 per alarm | Not created by default ([monitoring](../operations/monitoring.md#alarms-and-dashboard)) |
| AWS Budgets alert | $0 | A plain cost budget is free; recommended |
| Custom metrics | $0.30 per metric | None exist |
| DynamoDB point-in-time recovery | $0.20 per GB-month | Not offered: the current scope has no backups ([recovery](../operations/recovery.md#recovery-without-backups)) |

## Keeping costs down

- **Close mail clients you are not using.** Push connections are the largest cost that grows with use; each always-on client costs about $4.30 a month beyond the free allowance.
- **Set a budget alert** in AWS Budgets, sent to an address outside this mail system.
- **Expect storage to grow.** Deleting mail does not reduce S3 storage, by design.
- **Stay within your relay's plan.** `relay_quota` stops s3mail from sending more than the plan allows.
- **Region matters.** Prices differ slightly between SES receiving regions; recheck the main items for yours.
