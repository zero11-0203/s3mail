# Phase 0 evidence: REST API response streaming

[Documentation index](../README.md) · [Roadmap](../roadmap.md#phase-0--prove-the-risky-integrations)

**Status:** measured on 2026-10-16. This page records results, not requirements; the requirements they changed live in the [JMAP spec](../specs/jmap.md#push) and [deployment guide](../operations/deployment.md#design). The streaming runs used `AWS_IAM` authorization; a second round the same day put the routes behind the Cognito bearer-token authorizer ([common authorization contract](../specs/security.md#shared-endpoint-authorization)) and re-measured uploads with a real access token.

## Setup

- Regional REST API in `us-east-1` with three `AWS_PROXY` integrations in `STREAM` mode, invoking the `live` alias through `InvokeWithResponseStream`. Methods used `AWS_IAM` authorization (SigV4-signed requests from the owner), 5 rps / burst 10 throttling.
- Go Lambda on `provided.al2023` arm64, 256 MB, using `aws-lambda-go` v1.55.1 `events.APIGatewayProxyStreamingResponse` (JSON prelude, 8-null-byte delimiter, raw body) with no custom runtime code.
- Terraform 1.16.6, `hashicorp/aws` 6.69.1 (`response_transfer_mode`, `timeout_milliseconds`). Code: `go/internal/spike`, `go/cmd/spike-probe`, `infra/modules/spike-stream`.
- Client: `spike-probe` on a laptop in Japan over the public internet; single runs, not a load test.
- Authorizer round: Go `TOKEN` authorizer (`go/cmd/authorizer`, 128 MB arm64) with result caching off (TTL 0) and the identity validation expression `^Bearer [-0-9A-Za-z._~+/]+=*$`; an `UNAUTHORIZED` gateway response adding `WWW-Authenticate: Bearer resource_metadata="…"`; RFC 9728 metadata served by a public `MOCK` integration. Tokens came from `mail-auth token` (Cognito Essentials, 5-minute access token of 1,074 bytes). Each request carried a `?probe=` tag that the spike function logs, so CloudWatch Logs show which requests reached it.

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

### Authorizer round

| Check | Result |
| --- | --- |
| No `Authorization` header, `Bearer ` with nothing after it, `Basic …`, or a bare JWT without `Bearer` | Pass: 401 from the gateway's validation expression; the authorizer was not invoked |
| `Bearer not-a-jwt`, an unsigned (`alg` kept, signature stripped) token, a token with one signature character changed | Pass: 401 from the authorizer (`not a compact JWS`, `bad signature`, `bad signature`) |
| Every 401 | Pass: `WWW-Authenticate: Bearer resource_metadata="<stage URL>/.well-known/oauth-protected-resource"`, body `{"message":"Unauthorized"}` |
| Rejected requests reaching the spike function | Pass: none of the seven tags appear in its logs; it logged only the three authorized requests |
| Valid access token | Pass: 200, and the authorizer context (`sub`, `scope`) reached the integration |
| Protected resource metadata | Pass: 200 `application/json` naming the stage URL as `resource`, the Cognito issuer as the only authorization server, and the three wire scopes |
| Upload of 4,194,304 bytes with a real token | Pass: hash match, authorized |
| Upload of 4,194,305 bytes with a real token | Pass: 413 from the handler |
| Uploads of 4.5, 5 and 6 MiB with a real token | 413 from API Gateway; the authorizer ran and allowed each one first |

## Findings

1. The 29-second default integration timeout still caps total `STREAM` response time; set it to the 15-minute maximum.
2. A new API deployment took about a minute to take effect: one run immediately after an apply still hit the old timeout.
3. The gateway's binary upload limit falls between 4 MiB + 1 byte and 4.5 MiB with SigV4 or Cognito bearer headers alike. This matches Lambda's 6 MB (6,291,456-byte) request payload limit on the base64-encoded event: 4 MiB encodes to 5,592,408 bytes, leaving about 680 KiB for headers, authorizer context and the rest of the event, against a 1 KB access token. The 4 MiB `maxSizeUpload` fits.
4. Large streamed responses transfer slowly after the first 10 MB; size budgets such as the 64 MiB JSON response budget imply minutes of transfer.
5. The `TOKEN` authorizer's validation expression rejects missing and non-Bearer headers at the gateway without a Lambda invocation; malformed or forged bearer tokens cost one authorizer invocation and never reach the integration.
6. Oversized bodies are authorized before the payload limit rejects them, so each costs an authorizer invocation; gateway throttling, not the authorizer, bounds that cost.
7. Behind a stage path (`/spike`), the metadata URL is not the RFC 9728 well-known location derived from the resource URL, so clients must use the `resource_metadata` value from the challenge. A custom domain with no base path removes this difference.

## Remaining for this gate

- EventSource reconnect at token expiry, and whether aerc re-runs its `source-cred-cmd` when the 5-minute token expires; both need a JMAP endpoint. Push behavior is otherwise covered by later JMAP acceptance checks.
