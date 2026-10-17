# JMAP protocol, push and blobs

[Documentation index](../README.md) · [Architecture](../architecture.md) · [Roadmap](../roadmap.md)

## Scope and status

**Status: proposed; implementation and validation are pending.** Owns protocol coverage, discovery, request/response limits, push and blob transport. [Storage and sync](storage-sync.md) owns state consistency; [security](security.md) owns authorization; [search](search.md) owns text queries/snippets.

## Required behavior and invariants

- Advertised capabilities must satisfy their required RFC behavior.
- Polling remains a complete synchronization path when push is unavailable.
- Limits are enforced before work that cannot produce a valid response.
- Blob downloads require an authorized live reference and safe response headers.
- `Email/set` destruction never deletes original S3 objects; retained archive versions do not authorize normal JMAP access. Import/composition must durably archive complete MIME before acknowledging Email creation; see [retention](../operations/recovery.md#original-mail-retention).

## Design

The server targets the JMAP standards, not particular clients: implement the required behavior of RFC 8620 (Core), RFC 8621 (Mail) including submission, and advertise accurate capabilities and limits in the session object, so clients can adapt to the server rather than the reverse.

### Core capabilities to implement

Serve JMAP session discovery at `/.well-known/jmap`.

The implementation maintains a checked-in [conformance matrix](jmap-conformance.md), with an RFC section, scope/side-effect policy, limits and test for every method below. A capability is advertised only when its required behavior passes; unsupported optional behavior returns the specified error instead of silently succeeding.

| Capability / type | Required implementation coverage |
| --- | --- |
| Core | Session/discovery, `Core/echo`, request errors/result references, limits, upload/download, `Blob/copy`, EventSource; account checks also apply to copy |
| Mailbox | get, changes, query, queryChanges, set; roles, parent validation, rights and accurate email/thread counters |
| Email | get, changes, query, queryChanges where supported, set, copy, import, parse; complete required filters/sorts and MIME projection |
| Thread | get, changes; stable IDs and membership |
| Identity | get, changes, set with sender-policy enforcement; registry-owned identities cannot be arbitrarily removed |
| EmailSubmission | get, changes, query, queryChanges where supported, set; success hooks, cancellation and recipient delivery status |
| SearchSnippet | get with field-scoped text matching and safe output |

Methods and properties follow [JMAP Mail](https://www.rfc-editor.org/rfc/rfc8621.html); protocol-required behavior remains the acceptance criterion even when a reference client does not exercise it. WebSocket and optional push-subscription transports are not advertised until implemented.

`/set` and `Email/import` responses report `notCreated`, `notUpdated` and `notDestroyed` as `null` when nothing failed, which RFC 8620 §5.3 allows: browser clients test them for truthiness, and an empty object would read as a failure (the pinned webmail reported delivered sends as failed).

Support custom capabilities in session discovery and method dispatch, so project extensions can be added without changing core request handling. MIME projection includes text/HTML parts, inline content IDs and attachments. OAuth, per-invocation scope enforcement and registered-origin CORS follow [security](security.md); the matrix includes supported unread state, multi-mailbox membership, filters, sorting and pagination.

### Push

Clients learn about changes by polling `*/changes` or through push. Polling must always work; push makes web clients update live.

- **EventSource (RFC 8620 §7.3)** is served by a Go response-streaming Lambda through the same **Regional REST API Gateway** and authorizer as the other JMAP routes. REST APIs support `STREAM` proxy responses for up to 15 minutes; HTTP APIs do not provide this streaming path. See [API Gateway response streaming](https://docs.aws.amazon.com/apigateway/latest/developerguide/response-transfer-mode.html). Set every streaming integration's timeout to the 15-minute maximum (900,000 ms): the 29-second default still caps total `STREAM` response time. Phase 0 measured Go (`aws-lambda-go` streaming responses) and pinned-provider support; see the [streaming spike evidence](../evidence/phase0-streaming-spike.md). Poll the strongly consistent account state every three seconds, filter `StateChange` by `types` (including the push-only `EmailDelivery` type, whose state changes only when an Email is added; RFC 8621 §1.5), honor `ping`/`closeafter` (`closeafter=state` sends no initial event and ends after its first state event, so long-polling clients wait for a change), emit heartbeats within the gateway idle timeout (5 minutes for Regional endpoints) and stop at the earlier of token expiry, account disable, requested close or Lambda deadline. Reconnect requires a fresh valid token. Lease connections and release leases on disconnect, with expiry for crashes. Budget connected Lambda duration and reads rather than assuming idle push is free.
- **WebSocket push (RFC 8887)** is optional and deferred. If needed, it maps onto an API Gateway WebSocket API with connection IDs in DynamoDB and a DynamoDB Streams–driven Lambda that sends `StateChange` messages.
- The browser `EventSource` API cannot set an `Authorization` header. Clients that use it without a fetch-based replacement fall back to polling; do not accept tokens in query strings to work around this.

### Limits and blobs

- **Uploads:** set `maxSizeUpload = 4,194,304` bytes (4 MiB). A 6 MiB synchronous Lambda request must also fit base64 expansion, headers and the proxy event; a 5 MB binary cap is too large. Enforce the raw cap, verify binary media handling and test boundary sizes with realistic JWT/header lengths through the deployed gateway before advertising it. Phase 0 measured the gateway rejecting binary bodies of 4.5 MiB and above before invoking Lambda, with SigV4-sized headers. See [Lambda invocation limits](https://docs.aws.amazon.com/lambda/latest/dg/gettingstarted-limits.html).
- **JSON limits:** initially advertise `maxSizeRequest = 1,048,576`, `maxCallsInRequest = 16`, `maxObjectsInGet = 64`, `maxObjectsInSet = 16` and `maxMailboxesPerEmail = 8`; the [storage transaction contract](storage-sync.md#dynamodb-storage-and-consistency-contract) must support these limits. Bound body projections with JMAP `maxBodyValueBytes` and accurate `isTruncated` flags. API JSON responses also use the REST streaming integration, with a validated 64 MiB serialized response budget; preflight aggregate bodies/request combinations and return the appropriate size/limit error before committing work that would require an unreturnable response. Do not silently drop records or ignore requested body properties. Large bodies remain available through authorized blob downloads. Streamed responses beyond 10 MB are bandwidth-throttled, so large budgets imply long transfers.
- **Message size:** received raw MIME follows the [inbound size and parser policy](mail-flow.md#recipient-and-processing-policy). Limit new composed/imported outbound MIME to 20 MiB after MIME encoding, or a lower configured relay cap; advertise the actual mail capability limit. Apply independent header/recipient/part limits and verify total message size before submission acceptance, even when every attachment is individually small.
- **Downloads:** stream S3 data through the authorized download route; no dependency on presigned redirects. Set `X-Content-Type-Options: nosniff`, RFC-safe filenames and attachment disposition for active content. Do not serve untrusted HTML/SVG as a navigable page under the API or webmail origin. Inline rendering uses authenticated fetches into the client's isolated viewer; stored MIME is not rewritten to sanitize HTML. Expired upload blobs and destroyed-email references do not authorize downloads.
- Raising the upload cap is deferred to a measured follow-up ([roadmap](../roadmap.md)); any replacement path must preserve the same JMAP upload and authorization contract.

**Key engineering risk:** Building an interoperable JMAP server is likely the most complex component. Correct state/change semantics, threading, MIME handling, submission and client interoperability matter more than simply exposing a REST-shaped message API.

## Failure handling

Use the RFC-defined request, method and per-object errors. Expired/foreign states require full resynchronization under the [storage contract](storage-sync.md). Push closes at token expiry or disable; reconnect requires valid credentials. Optional transports and unproven capabilities remain unadvertised.

## Acceptance checks

- **RFC conformance suite:** black-box HTTP tests organized by RFC 8620/8621 section: session discovery, method-level and request-level errors (`stateMismatch`, `cannotCalculateChanges`, `requestTooLarge`, …), result references, advertised limits being enforced. RFC examples serve as golden fixtures.
- **Push:** state/search-ready notifications, `EmailDelivery` only on added Emails, filters, pings, closeafter, heartbeats, lease cleanup, account disable and reconnect at token expiry or the 15-minute ceiling.
- Verify deployed binary upload boundaries, aggregate JSON/body budgets, safe download headers and stream termination with realistic JWT/header sizes.
- Run the [authorization matrix](security.md#acceptance-checks), [sync model](storage-sync.md#acceptance-checks), [search checks](search.md#acceptance-checks) and [client replays](clients.md#acceptance-checks) before advertising dependent capabilities.
