# Clients and deferred webmail

[Documentation index](../README.md) · [Architecture](../architecture.md) · [Roadmap](../roadmap.md)

## Scope and status

**Status: proposed; implementation and validation are pending.** Owns reference-client support and display safety. aerc is the initial client. Browser hosting and its acceptance checks apply only in Phase 4.

## Required behavior and invariants

- The API follows JMAP standards; client-specific quirks do not redefine it.
- Core development, testing and cutover use aerc and CLI/protocol helpers.
- Webmail is an optional, later deployment on Lambda with Web Adapter.
- Client display protections must treat message and snippet content as untrusted.

## Design

The server is client-agnostic: any JMAP client that implements the [JMAP contract](jmap.md) and supports [OAuth 2.0 bearer tokens](security.md#authentication-and-authorization) should work. The design does not depend on any particular client, and the server does not special-case client behavior.

### Requirements for a client

- Speaks JMAP Core and Mail (and Submission, to send).
- Obtains a token through OAuth authorization code + PKCE against a registered Cognito app client, or accepts a bearer token from an external command.
- For browser clients: served from an origin registered in the CORS policy and in its Cognito app client's redirect URIs.

### Reference clients

**[aerc](https://aerc-mail.org/) is the initial reference client.** Use it with `mail-auth` for the Phase 0 vertical slice, ongoing acceptance and Phase 3 primary-mail cutover. The protocol conformance suite and two independent CLI sessions test interoperability and shared state without requiring a browser client. Browser deployment and its acceptance checks begin in Phase 4, after the core system is operational.

| Client | Priority / stage | Acceptance scope |
| --- | --- | --- |
| aerc | First; Phases 0–3 and every core release | OAuth helper/refresh, reading, drafts, moves/keywords, search, attachments, sending and lost-response reconciliation |
| JMAP webmail (candidate: `root-fr/jmap-webmail`) | Lower priority; Phase 4 | Browser OAuth/session flow, CORS, client display safety, streaming/proxy behavior and cross-client state |

These are replaceable test targets. Passing aerc tests does not replace the RFC conformance matrix, and the server remains client-agnostic while browser-specific integration is deferred.

### Browser hosting and client safety

**Deferred to Phase 4:** Run the selected upstream Next.js webmail as a **container on on-demand Lambda with AWS Lambda Web Adapter**. The adapter forwards Lambda HTTP events to the application's ordinary Node.js server, so retain its existing standalone deployment model. Build a thin deployment image from pinned upstream sources/images plus a pinned adapter release, push it to a private ECR repository and deploy Lambda by image digest through Terraform. Use the container-compatible Event Function path; no continuously running host or provisioned concurrency is planned initially. See [Lambda Web Adapter and its Next.js examples](https://github.com/aws/aws-lambda-web-adapter).

Serve `webmail.example.com` through a dedicated Regional REST API Gateway with ACM TLS and `AWS_PROXY` integration. The webmail gateway's login, public assets and OAuth callback routes must be reachable before login; its own session checks protect authenticated application routes. Do not attach the JMAP bearer authorizer to every webmail request, which would prevent login. JMAP endpoints retain their separate authorization contract ([security](security.md#authentication-and-authorization)). Configure Lambda Web Adapter for response streaming and match the gateway integration's transfer mode; test proxying if the pinned webmail routes JMAP downloads or SSE through its server. Do not introduce a direct public function URL that bypasses gateway controls. Add gateway throttling, bounded Lambda concurrency and a webmail-specific budget alert when enabling this component.

The webmail Lambda is stateless across invocations: OAuth state/PKCE and refresh sessions must survive requests landing on different execution environments. Prefer the pinned client's protected cookie/session mechanism with a common encryption/signing key; any required shared server-side session state needs an explicit backing store. Keep session secrets in Secrets Manager, set out of band, and let the dedicated execution role read only those named secrets plus write its logs. Mail access uses the user's OAuth session through JMAP; the webmail role has no S3 mail/index or DynamoDB mailbox read grants. In-memory caches and `/tmp` are disposable; redirect necessary Next.js cache/temp writes to `/tmp` and do not depend on durable local files. Lambda's container filesystem requirements are documented in [Lambda container images](https://docs.aws.amazon.com/lambda/latest/dg/images-create.html).

When Phase 4 starts, pin the upstream commit, adapter release and image digest in a version lock file; never deploy a floating `latest` tag. Configure the application/adapter port and a verified readiness route, `JMAP_SERVER_URL`, Cognito issuer/client, exact scopes, callback/logout URLs, PKCE and OAuth-only mode for that version. Treat the server as trusted to process mail/session tokens whether individual requests are browser-direct or proxied. Record the actual token/request path, disable unnecessary credential persistence and exclude mail/tokens from logs. See [the upstream deployment/OAuth configuration](https://github.com/root-fr/jmap-webmail). Measure cold starts and image/memory size before considering paid warm capacity.

**Client responsibilities:** HTML sanitization, sandboxed message display, blocking remote tracking images and safe snippet rendering belong to clients. The backend preserves raw MIME and implements recipient disposition, parsing limits, blob authorization and safe download headers ([mail flow](mail-flow.md#design)–[JMAP](jmap.md#design)). CLI acceptance covers the terminal client's MIME/external-content behavior; browser HTML/sandbox checks become mandatory when webmail is enabled.

## Failure handling

A client incompatibility must be fixed or documented; it cannot weaken the shared protocol/security contract. A failed Phase 4 browser gate leaves webmail disabled. CLI access remains the independent test and access path.

## Acceptance checks

- **Client libraries as drivers:** initially use aerc's pinned JMAP library (verify `go-jmap` by rockorager for the chosen release) plus raw protocol tests. In Phase 4, add a JavaScript client driver and browser tests for the selected webmail. The RFC conformance suite covers required behavior independently of either client.
- **Record and replay:** capture and scrub aerc HTTP sessions first; add browser recordings when webmail is deployed. Use two independent CLI sessions plus the model tests to exercise shared mailbox state before that stage.
- **Owner acceptance checklist** ([deployment](../operations/deployment.md#design)) initially covers aerc login/refresh, reading, moves, drafts, attachments, Sent hooks, response-loss reconciliation and search; the protocol helper tests push where needed. Terminal rendering/external-content behavior is checked on aerc. Phase 4 adds browser login/logout/cookies, cross-instance sessions, HTML sanitization/sandboxing, blocked remote images, safe snippets and CLI/browser synchronization. Browser-only checks do not gate the initial CLI release.

### Browser release gate (Phase 4)

Validate successful login/refresh/logout across cold starts and different instances, protected cookie behavior, disabled caching of authenticated pages/responses, redirects, CORS, assets, request-size boundaries, attachments and streamed/proxied responses. Re-run mail/send/search acceptance and shared-state checks with aerc. Webmail is enabled only after its own gate passes; failure leaves the working CLI/mail backend usable and does not retroactively block the earlier core cutover. Preserve the previous image digest/version for alias rollback.
