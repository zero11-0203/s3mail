# Phase 0 evidence: REST API response streaming

[Documentation index](../README.md) · [Roadmap](../roadmap.md#phase-0--prove-the-risky-integrations)

**Status:** measured on 2026-10-16. This page records results, not requirements; the requirements they changed live in the [JMAP spec](../specs/jmap.md#push) and [deployment guide](../operations/deployment.md#design). It covers the streaming half of the Phase 0 REST API proof only: the spike had no Cognito authorizer, so the [common authorization contract](../specs/security.md#shared-endpoint-authorization) and realistic JWT header sizes remain unproven.

## Setup

- Regional REST API in `us-east-1` with three `AWS_PROXY` integrations in `STREAM` mode, invoking the `live` alias through `InvokeWithResponseStream`. Methods used `AWS_IAM` authorization (SigV4-signed requests from the owner), 5 rps / burst 10 throttling.
- Go Lambda on `provided.al2023` arm64, 256 MB, using `aws-lambda-go` v1.55.1 `events.APIGatewayProxyStreamingResponse` (JSON prelude, 8-null-byte delimiter, raw body) with no custom runtime code.
- Terraform 1.16.6, `hashicorp/aws` 6.69.1 (`response_transfer_mode`, `timeout_milliseconds`). Code: `go/internal/spike`, `go/cmd/spike-probe`, `infra/modules/spike-stream`.
- Client: `spike-probe` on a laptop in Japan over the public internet; single runs, not a load test.

## Results

| Check | Result |
| --- | --- |
| Server-sent events, 13 events 30 s apart (6 min 1 s) | Pass: every event arrived as sent, maximum lag 139 ms, first byte 0.7 s |
| Streams over 29 s with the default integration timeout | Fail: cut with an HTTP/2 `INTERNAL_ERROR` at about 30 s although the Lambda completed normally |
| Same, with `timeout_milliseconds = 900000` | Pass (31 s, 36 s, 41 s and the 6-minute run) |
| 25 MiB download, `Content-Length` set | Pass: SHA-256 match in 31.6 s (about 0.8 MB/s overall; the documented throttle above 10 MB is 2 MB/s) |
| `Content-Type`, `Content-Disposition`, `X-Content-Type-Options: nosniff` | Pass: passed through unchanged |
| Binary upload of 4,194,304 bytes | Pass: arrived base64-encoded via `binary_media_types`, hash match |
| Binary upload of 4,194,305 bytes | Pass: rejected with 413 by the handler |
| Binary uploads of 4.5, 5 and 6 MiB | Rejected with 413 by API Gateway before the Lambda |

## Findings

1. The 29-second default integration timeout still caps total `STREAM` response time; set it to the 15-minute maximum.
2. A new API deployment took about a minute to take effect: one run immediately after an apply still hit the old timeout.
3. With SigV4 headers, the gateway's binary upload limit fell between 4 MiB + 1 byte and 4.5 MiB, so the 4 MiB `maxSizeUpload` fits with a small margin. Re-measure with realistic Cognito JWT sizes.
4. Large streamed responses transfer slowly after the first 10 MB; size budgets such as the 64 MiB JSON response budget imply minutes of transfer.

## Remaining for this gate

- Authorizer-protected streaming, including rejection of invalid tokens before a stream opens.
- Upload boundaries with realistic JWT/header sizes.
- EventSource reconnect at token expiry; push behavior is otherwise covered by later JMAP acceptance checks.
