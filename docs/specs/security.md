# Identity, authorization and least privilege

[Documentation index](../README.md) · [Architecture](../architecture.md) · [Roadmap](../roadmap.md)

[OAuth and endpoint authorization](#authentication-and-authorization) · [IAM roles](#iam-role-requirements) · [CI identity](#ci-identity) · [Acceptance](#acceptance-checks)

## Scope and status

**Status: proposed; implementation and validation are pending.** Owns OAuth/token policy, endpoint and method authorization, all IAM role requirements and CI trust. Applies to Phase 0, runtime, operations, deployment and future webmail.

## Required behavior and invariants

- Authorize each operation from the verified subject and mapped account; an ID is never authorization.
- Every created role uses the least privilege needed for its current responsibility.
- CI has no direct personal-mail reads or runtime-secret access.
- Encrypt S3 (including indexes) and DynamoDB at rest; use TLS on public endpoints.
- Use short-lived AWS credentials and local OS keychain storage; keep owner recovery methods outside this mailbox.

## Design

### Authentication and authorization

Use **one Cognito User Pool and one user**, with self-service registration disabled. **MFA is disabled by owner decision:** sign-in is a single password with an 8-character minimum and no composition rules. This is an accepted risk: it is below NIST SP 800-63B's 15-character guidance for single-factor passwords, the pool tier has no breached-password check, and a phished or reused password grants full mailbox and send access until the session is revoked. Use a unique, randomly generated password; reconsider MFA or passkeys before primary-mail cutover. **OAuth 2.0 bearer tokens are the only accepted credential** for the JMAP API; there is no Basic authentication and no app passwords. Every client maps to the same JMAP account.

### Client registration and discovery

- **One Cognito app client per client application**, each a public client using authorization code + PKCE with its own redirect URIs. Initially provision only the native/terminal app client for `mail-auth` and aerc; add the webmail app client, callback/logout URLs and CORS origin in Phase 4. Cognito has no dynamic client registration, so adding a new client application is a Terraform change, not a code change.
- **Discovery:** publish OAuth protected resource metadata (RFC 9728) at `/.well-known/oauth-protected-resource`, naming the Cognito issuer as the authorization server, and return `WWW-Authenticate: Bearer` with a `resource_metadata` parameter on `401` responses, so clients with built-in OAuth can find the authorization server without hard-coded settings. Cognito publishes OIDC discovery at its issuer URL.
- **Clients without built-in OAuth** can still connect if they accept a bearer token from an external command (as aerc does); `mail-auth` provides that token.

### Browser login

This flow is introduced with webmail in Phase 4 and is not required for initial CLI acceptance.

1. The browser client starts an OAuth authorization-code flow with PKCE against its own Cognito app client.
2. Cognito authenticates the user and returns tokens.
3. The client supplies an access token when calling the JMAP API.
4. The gateway's Lambda authorizer and the handlers use the common validation contract below; handlers enforce each method's scopes and account/blob ownership.
5. The backend derives the permitted JMAP account from the verified user identity, **not from an arbitrary client-supplied account ID**.

### Terminal login

1. A local helper such as `mail-auth login` uses OAuth authorization-code + PKCE.
2. It stores refresh credentials securely in the OS keychain.
3. `mail-auth token` supplies a valid access token to any client that can run a token command (such as aerc) without embedding secrets in configuration files.
4. Verify session expiry and refresh behavior with aerc from Phase 0. The helper may open the Cognito login page in the system browser; this does not require a hosted webmail client.

Use five-minute access tokens and refresh-token rotation where supported by the pinned clients. Keep the Cognito password and the AWS/registrar/relay recovery methods outside this mailbox; Cognito account recovery is admin-only through the owner's AWS access. Browser refresh credentials follow the selected client's session design ([clients](clients.md#design)) and must never enter URLs or telemetry.

### Shared endpoint authorization

All JMAP API, session, upload, streamed download and push routes use the same bearer-token verifier. Verify signature and allowed algorithm, trusted issuer/JWKS, `token_use=access`, expiry/not-before, allowlisted Cognito `client_id` and any configured resource audience. ID tokens are rejected. Cache trusted signing keys and refresh on an unknown `kid` with rate limiting. A gateway identity validation expression may reject headers that are not `Bearer <token>` before the authorizer runs; it never replaces verification. Disable API Gateway authorizer-result caching initially; handlers still check each operation's scope and the subject-to-account mapping. Cross-account object IDs and guessed blob IDs never grant access. Downloads authorize the owning account and a live email reference or an unexpired owned upload before opening S3.

**Revocation policy:** Revoking a Cognito refresh token stops future refreshes, but offline JWT validation does not detect that revocation. This design accepts at most the remaining five-minute access-token lifetime for an already-issued token. Emergency account/client disable flags live in a separate security table, cached for at most 30 seconds by all handlers; push rechecks that cache on each state poll and closes on disable or token expiry. This behavior is tested explicitly. See [Cognito token verification and revocation](https://docs.aws.amazon.com/cognito/latest/developerguide/amazon-cognito-user-pools-using-tokens-verifying-a-jwt.html).

**Public endpoints:** Use the Regional REST API for every JMAP route, including `STREAM` response integrations for downloads/push. No direct function URLs bypass the authorizer or gateway throttles. Start with 5 requests/second and burst 10 for the interactive API, four leased push connections per account and reserved push concurrency of four; tune only after measuring reference-client behavior. Reject invalid credentials before S3/index downloads or stream establishment. CORS allows only registered origins, including Authorization preflights; the relay webhook has its own shared-secret authentication and queueing route.

**Compatibility validation:** A client is supported if it can obtain or accept an OAuth bearer token and present it to the JMAP API. Confirm this early for each reference client. Many JMAP clients are developed against Stalwart or Fastmail and may assume server-specific behavior; treat such gaps as client bugs or documented incompatibilities, not as reasons to add server-side special cases.

### Scopes

Logical permissions are `mail.read` (get/query/changes/parse/snippets/download/push), `mail.write` (Email/Mailbox/Identity mutations, upload/import/copy) and `mail.submit` (submission creation/cancellation/destruction). The Cognito resource-server identifier is `s3mail`, so requested and validated wire scopes are exactly `s3mail/mail.read`, `s3mail/mail.write` and `s3mail/mail.submit`; never match suffixes. See [Cognito custom scopes](https://docs.aws.amazon.com/cognito/latest/developerguide/cognito-user-pools-define-resource-servers.html).

Interactive clients receive all three. Authorize each invocation in a batched request, including result references and implicit side effects. An `EmailSubmission/set` with Email success hooks requires both submit and write permission before accepting it; an unauthorized hook cannot cause a send. Object reads and blob references additionally require read access. A checked-in operation/side-effect matrix covers every advertised method and endpoint. Narrower property-level permissions can be added later through the same dispatcher without changing existing scope meanings.

### IAM role requirements

**Every IAM role created for this project must follow least privilege**, including Phase 0 proofs of concept, runtime/service roles, CI roles, scheduled jobs, recovery and the later webmail. Maintain a checked-in permission inventory identifying each role's purpose, trusted principal, required AWS API operations, exact resources and documented exceptions. Follow [AWS IAM least-privilege guidance](https://docs.aws.amazon.com/IAM/latest/UserGuide/best-practices.html).

- **Separate responsibilities:** give each Lambda with a distinct responsibility its own execution role. Keep runtime, deployment and owner-recovery roles separate. Sharing a binary does not justify sharing a broad role; derived-data cleanup and work-recovery jobs have separate permissions.
- **Allow exact actions:** derive customer-managed/inline allowlists from actual SDK/runtime/event-source operations. Do not attach `AdministratorAccess`, `PowerUserAccess`, service `FullAccess` or account-wide `ReadOnlyAccess` policies. Avoid action wildcards. A permissions boundary is a ceiling, not a replacement for a narrow role policy.
- **Original-mail protection:** relevant roles, including ingestion, API/download, submission, indexing, maintenance/GC, CI deployment and owner recovery, have no `s3:DeleteObject`, `s3:DeleteObjectVersion` or `s3:DeleteBucket` grants on the original bucket/resources, including through broader managed policies. Grant version reads only where needed and object creation only to original producers. GC delete grants name disposable buckets/prefixes only. The original bucket policy also denies object/version deletion, bucket deletion, lifecycle and versioning changes to every principal, so removing an original first requires deleting that policy; see [retention](../operations/recovery.md#original-mail-retention).
- **Constrain resources:** use the specific queue/topic/bus, table/index/stream, function alias, secret, KMS key, bucket/object prefix and log group. Separate read/write/delete/administration statements. Scope S3 listing with prefix conditions and object/version operations with object ARNs. Pre-create log groups through Terraform and permit runtime log writes only to the function's own group. Runtime roles cannot administer IAM or infrastructure.
- **Document unavoidable wildcards:** allow `Resource: "*"` only for explicitly named actions that AWS cannot scope to resource ARNs, with the reason and supported conditions recorded. Unknown resource names are not a reason for broad access. Grant only the necessary cryptographic operations on named KMS keys when encryption requires them, with compatible service/encryption-context conditions; readers do not receive key administration.
- **Restrict role assumption/passing:** runtime roles have no IAM mutation, `iam:PassRole` or arbitrary `sts:AssumeRole`. The infrastructure role can pass only the explicitly declared runtime/service role ARNs to their intended services, using `iam:PassedToService`. It cannot pass CI/bootstrap/recovery roles. See [PassRole restrictions](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_use_passrole.html).
- **Scope trust and resource policies:** trust only the actual assuming service or exact federated repository/environment subjects. Restrict SES/SNS/SQS and gateway-to-Lambda resource-policy grants to the intended producer account/source using supported `aws:SourceAccount`/`aws:SourceArn` conditions. Verify which context keys each service supplies; unsupported trust conditions must not prevent execution-role assumption. Lambda outbound calls may also use supported `lambda:SourceFunctionArn` identity-policy conditions, which differ from incoming invocation conditions; see [Lambda source-function controls](https://docs.aws.amazon.com/lambda/latest/dg/permissions-source-function-arn.html).
- **Grant only current needs:** create roles/grants only for enabled components. Webmail/ECR rights arrive in Phase 4. Human recovery uses separately scoped short-lived owner sessions that CI/runtime roles cannot assume. Review access activity and remove unused permissions as responsibilities change.

The permission inventory must implement these component boundaries; a service listed here does not mean permission to all its actions/resources:

| Component | Required access | Access excluded |
| --- | --- | --- |
| Authorizer | Read security/account mapping records and write its logs | Mail bodies, mailbox writes, relay credentials |
| Ingestion | Consume its queue, read received MIME versions, write parsed objects and conditional receipt/mail transactions | SMTP secrets, index publication |
| Dispatcher | Read/acknowledge outbox/stream work, publish to named queues/event bus | MIME/index objects, relay credentials |
| Indexer / search | Separate roles: indexer reads source data and publishes index/generation; search reads index/live metadata | Search cannot publish or mutate mail; neither gets relay credentials |
| JMAP / blob download / push | Separate roles: API performs mail/upload/submission transactions and invokes the search alias; download reads authorized blobs; push reads state/security and manages connection leases | Relay credentials, original-object deletion and infrastructure administration |
| Submission worker | Consume its queue, read frozen send material and its named SMTP secret, update attempts/quotas/submission state | Other secrets, arbitrary object deletion |
| Webhook receiver / bounce worker | Separate roles: receiver reads its named webhook secret and enqueues; worker consumes events and updates delivery/correlation state | SMTP credentials, raw message bodies, index publication |
| GC / work recovery | Separate roles for disposable-data deletion prefixes or queue/attempt recovery | Original-object/version deletion; a combined all-purpose maintenance role; SMTP handoff from a recovery sweep |
| Webmail (Phase 4) | Read named session secrets and write its logs; access mail through user OAuth/JMAP | Direct AWS mail/index reads, SMTP credentials, ECR image push |
| CI plan / code / infrastructure | Separate duties and target resources in the CI table below | Personal-mail reads, runtime secrets, owner recovery and changes to CI/bootstrap policy |

**Shared-table limitation:** the initial mail table places typed records under one account partition ([storage model](storage-sync.md#design)). Table/action permissions and supported `dynamodb:LeadingKeys` conditions constrain partition access, not record types by sort-key prefix. Do not claim that IAM isolates ingestion/dispatcher writes from every other record in that same partition. Grant only needed item actions, enforce the transaction invariants in code and record this trusted-backend boundary. A component needing an independently enforced record boundary must use a separate table/partition with compatible conditions. See [DynamoDB fine-grained conditions](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/specifying-conditions.html).

### CI identity

GitHub Actions authenticates to AWS with **OpenID Connect**: each job exchanges a short-lived GitHub-issued token for temporary AWS credentials. No AWS access keys exist in GitHub. The OIDC provider, all CI roles and their policies/boundary are created by `bootstrap`; main deployment roles cannot change them.

**Bootstrap** (applied by hand, once, with a short-lived owner admin session, such as IAM Identity Center (`aws sso login`) or console-credential sign-in (`aws login`); never long-lived access keys):

1. IAM OIDC identity provider for `https://token.actions.githubusercontent.com`, audience `sts.amazonaws.com`.
2. Terraform state bucket (versioned, encrypted, public access blocked) and the artifact bucket.
3. The plan, code-deploy and infrastructure-deploy roles and the permissions boundary below.
4. Bootstrap starts with local state; after the first apply, its state is moved into the new bucket with `terraform init -migrate-state`.

**GitHub environments:** `production` (no reviewers) is used by the build, code-only apply and post-deploy jobs on `main`; the plan job declares no environment, so its branch subject cannot assume either deployment role; `production-gated` (required reviewer: the owner) is used by the apply job when the plan contains more than code-version changes ([CI/CD](../operations/deployment.md#cicd) step 3).

**Roles:**

| Role | Trusted subject (`token.actions.githubusercontent.com:sub`) | Permissions |
| --- | --- | --- |
| `ci-plan` | `repo:<owner>/<repo>:ref:refs/heads/main` (the plan job on `main`, which declares no environment) | Project control-plane describe/list/get configuration only; read exact Terraform state keys and list their prefix; write/delete the exact lock key; no mailbox data-plane reads |
| `ci-code-deploy` | `repo:<owner>/<repo>:environment:production` | Plan-role reads/state locking plus artifact-prefix writes and named deployed Lambda code publication/alias updates; webmail added in Phase 4; canary invocation; no infrastructure configuration/IAM/DNS changes |
| `ci-infra-deploy` | `repo:<owner>/<repo>:environment:production-gated` | Project-scoped infrastructure deployment policy below; same state/artifact access restrictions |

Every trust policy requires `token.actions.githubusercontent.com:aud = sts.amazonaws.com` and exact repository/subject matches. A job declaring an environment presents an environment subject; configure both production environments to permit deployments only from `main`, in addition to branch/workflow protection. The gated role is not assumable through the ungated environment. Explicit job conditions exclude fork PRs from all AWS-authorized jobs; do not rely on the trigger name or an assumption that OIDC can never be requested by a fork workflow.

The plan role uses a customer-managed allowlist, **not AWS-managed `ReadOnlyAccess`**, which includes S3 object and DynamoDB item reads. Explicitly deny raw-mail and index object/version reads, mailbox table GetItem/BatchGetItem/Query/Scan/PartiQL reads, mail-key decryption, secret values and arbitrary Lambda invocation. Terraform may read infrastructure definitions and its own secret-free state, not personal data. Grant new read actions only after checking their data exposure; tests exercise denied synthetic data reads under each CI role. See [the ReadOnlyAccess policy](https://docs.aws.amazon.com/aws-managed-policy/latest/reference/ReadOnlyAccess.html).

Example infrastructure-role trust condition:

```json
{
  "Effect": "Allow",
  "Principal": { "Federated": "arn:aws:iam::<account>:oidc-provider/token.actions.githubusercontent.com" },
  "Action": "sts:AssumeRoleWithWebIdentity",
  "Condition": {
    "StringEquals": {
      "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
      "token.actions.githubusercontent.com:sub": "repo:<owner>/<repo>:environment:production-gated"
    }
  }
}
```

**Infrastructure-deploy permissions:** Terraform creates IAM roles for the Lambdas, so an unrestricted deploy role could grant itself administrator access. Instead:

- **Service actions:** allow only exact provisioning/configuration actions required by the declared resources, scoped under the IAM requirements above. The project's service list is not permission to grant `service:*`. Add the webmail repository's required ECR configuration/image-push actions only in Phase 4, with repository-scoped grants and a documented exception for authorization actions that cannot be resource-scoped. Bootstrap these policy additions before enabling webmail. Direct mailbox/index reads and mail-key decryption remain denied.
- **Route 53** changes limited to the project's hosted zone.
- **IAM:** required role/policy-management actions are scoped to declared runtime roles under `/s3mail/`; creation and supported updates require the fixed project permissions boundary. Attach only declared project policy ARNs. Prohibit removing/replacing the boundary or modifying CI/bootstrap/recovery roles and the OIDC provider. `iam:PassRole` uses an explicit role-ARN allowlist plus intended `iam:PassedToService` values, rather than every role under the path. Check condition support per action before deployment.
- The **permissions boundary** caps every role Terraform creates at the project's services and resources, so even a mistaken or malicious role definition cannot exceed it.
- No backup-specific roles or grants in the current scope. No `secretsmanager:GetSecretValue`: Terraform creates empty secrets; values are set out of band ([Terraform conventions](../operations/deployment.md#terraform-conventions)).

**Deployment trust:** A code deployer can replace a Lambda that legitimately reads mail/secrets, so it has indirect authority over that data even without direct read grants. Treat code on `main`, dependencies, workflows and release artifacts as trusted production inputs; only the owner pushes to `main`, external changes to workflow/bootstrap paths need owner review before the owner lands them, and verify artifact hashes against the release manifest. The IAM boundary limits account-wide escalation, not the application data authority inherent in deploying its code. From Phase 4, apply the same trust and digest verification to the webmail image, which handles the user's session tokens.

**Workflow side:** jobs that use AWS declare `permissions: id-token: write, contents: read` and call `aws-actions/configure-aws-credentials` (pinned to a commit SHA) with the role ARN. Role ARNs and the AWS region are stored as **GitHub repository variables**, not in workflow files, because account IDs must not appear in the public repository.

**Later additions:** any separately approved agent infrastructure adds its required ECR/AgentCore permissions to the gated infrastructure role and boundary through bootstrap.

## Failure handling

Reject invalid credentials before expensive data access or stream setup. Account/client emergency disable is enforced on active requests and push polls. The revocation window and shared-table IAM boundary above are explicit limits, not isolation guarantees. Independent owner/identity recovery follows [recovery](../operations/recovery.md#failure-handling).

## Acceptance checks

A table-driven matrix runs every method/endpoint against no token (`401` with discovery challenge), malformed/expired/wrong-issuer/wrong-client/ID tokens, exact missing `s3mail/...` scopes, another account and Basic credentials. Include positive authorized calls, mixed-scope request batches, submission hooks without write permission, guessed blob IDs, destroyed references, CORS preflights and session disable during push. Test the explicit five-minute Cognito revocation window and 30-second emergency-disable cache, including reconnects.

Run the full matrix locally and its tokenless/negative rows in CI against AWS; the owner positive checklist covers real successful credentials. Real infrastructure tests verify 4 MiB upload boundaries, streaming body/attachment responses and five-minute token reconnects. Capture logs/metrics/spans, including proxy, SDK, SMTP and webhook errors, to assert no mail content, addresses or tokens escape. IAM tests prove CI can plan/deploy permitted resources while data-plane reads fail.

- **IAM least privilege:** validate every created role's trust/permission policies with IAM Access Analyzer and repository policy checks. Reject broad managed/full-access policies, action wildcards, undocumented wildcard resources, unbounded PassRole and unsupported conditions. Test required operations and denied cross-component access on synthetic resources, accounting explicitly for the shared-table limitation. Review actual access activity and remove unused grants; a successful deploy alone does not prove least privilege.

- **Original-delete denials:** inspect effective policies, including managed policies and resource grants, for all relevant roles. On synthetic originals, single-object, batch and version-specific deletes must fail; authorized version reads and disposable GC must still work.
