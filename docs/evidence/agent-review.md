# Mail agent design and implementation review

[Documentation index](../README.md) · [Agent specification](../specs/agent.md) · [Agent phases](../roadmap.md#agent-phases)

**Reviewed:** 2026-10-18, commit `11ce593`. This is a review of the checked-in design and A0 implementation, not a deployed acceptance result. No findings were fixed as part of this review, and no release gate is marked passed.

## Assessment

The architecture has a sound boundary: the agent is an ordinary JMAP client, the A0 token is read-only, and the runtime policy grants no direct access to mail storage. Typed tools, a second JMAP method allowlist, escaped untrusted-data envelopes, separate infrastructure state and immutable image digests are useful safeguards. The original-mail retention and uncertain-submission rules remain owned by the mail service.

A0 still needs changes before its answers and recovery behavior can be trusted. Citation validation accepts answers with no evidence, and a transient tool failure can leave a broken conversation in persistent state. Resource limits, login callback binding and release promotion also have gaps. Passing the existing offline tests does not establish the question, injection, telemetry or deployed identity gates.

The early A0 deployment is explicitly documented as owner-requested. This review does not treat that exception, the absence of A1–A4 features, or deferred browser work as defects.

## Scope and method

Reviewed the owning [agent specification](../specs/agent.md), [security contract](../specs/security.md), roadmap, deployment and monitoring guidance; `agent/src`, its tests and packaging; `go/internal/agentcli` and `go/cmd/agent`; the `infra/agent` root, `infra/modules/agent`, Cognito registration, permission inventory and checks workflow. Relevant Go JMAP and MIME paths were inspected to check tool assumptions.

Findings labeled **reproduced** used synthetic fixtures, the installed locked dependencies and temporary local probes. Other findings are explicitly source or design review. No personal mail, AWS data-plane calls, live model requests, Terraform plans or applies were used. AWS documentation was consulted for Identity session binding, Memory integration and log retention.

Severity: **High** means a central evidence or recovery guarantee is broken; **Medium** means a concrete functional, security or operational gap; **Low** means documentation alignment is needed. These are priorities within this personal, single-owner system.

| ID | Severity | Finding | Evidence |
| --- | --- | --- | --- |
| F1 | High | Uncited and malformed-citation answers pass | Reproduced |
| F2 | High | Transport failure leaves unanswered tool calls in saved history | Reproduced |
| F3 | Medium | Long threads exceed JMAP limits before the local cap applies | Reproduced with strict fake; server source confirms limit |
| F4 | Medium | Token budget is retrospective and final overruns pass | Reproduced; context growth confirmed in source |
| F5 | Medium | Login callback is not correlated with the initiating login | Source and existing callback test |
| F6 | Medium | `live` follows a new runtime version before smoke testing | Terraform and deployment source |
| F7 | Medium | Runtime log retention is unmanaged | Terraform source |
| F8 | Medium | CLI prints untrusted model output as terminal controls | Source |

## Findings

### F1. Citation validation accepts answers without evidence

**Where:** [graph.py](../../agent/src/s3mail_agent/graph.py), `check_citations` (lines 191–201); [citations.py](../../agent/src/s3mail_agent/citations.py), `CITATION` and `invalid_citations`.

The validator only rejects recognized citation IDs outside `fetched_ids`. An answer containing no recognized citations produces an empty invalid-ID list and passes. A malformed citation such as `[email:bad.id]` also disappears from validation. This violates the spec's requirement that every mail claim cite evidence fetched in the current run.

**Reproduced:** with the existing scripted model and no tool calls, both `Your invoice is 999 EUR.` and `Your invoice is 999 EUR [email:bad.id].` returned `status: ok`, `tool_calls: 0`, `citations: []`. No invoice was fetched. The current tests exercise invented IDs with valid syntax, not omitted or malformed citations.

**Impact:** the owner can receive unsupported mail claims presented as successfully checked answers. On follow-ups, old checkpoint content can also be reused without a current read merely by omitting citations.

**Fix direction:** use a structured answer contract that distinguishes evidence-backed claims from an explicit no-results/clarification response. Validate required evidence and reject malformed citation markers before rendering. Keep ID membership checking, but do not mistake it for checking that the cited message supports a claim. Add omission, malformed-marker, unsupported-claim and stale-follow-up cases to the question evaluation and graph tests. Legitimate no-results responses must remain possible.

### F2. A JMAP transport failure corrupts the next conversation turn

**Where:** [jmap.py](../../agent/src/s3mail_agent/jmap.py), `_send` (lines 97–107); [graph.py](../../agent/src/s3mail_agent/graph.py), `tools` (lines 164–180) and `run_ask`; [app.py](../../agent/src/s3mail_agent/app.py), `Handler`.

`httpx.ReadTimeout` and other transport exceptions are not converted to `JmapError`. They escape the tool node, so its failure responses are never appended. The preceding AI tool-call message has already been checkpointed. `Handler` returns `internal`, and the next invocation supplies new input to the graph, adding a new human message to that incomplete history.

**Reproduced:** force a POST timeout after the model calls `list_mailboxes`, then ask another question using the same `InMemorySaver` and thread. The first result is `{"status":"error","error":"internal"}`. The next model input has roles `system, human, ai, human`; the AI call `t1` still has no corresponding tool response. The existing scripted model accepts this transcript, masking a protocol error that a real tool-calling provider can reject.

**Impact:** a temporary network problem can make a persisted thread unusable on subsequent turns. This also contradicts the documented crash/resume behavior: submitting new input is not the same operation as resuming pending work from a checkpoint.

**Fix direction:** normalize expected transport failures into bounded, content-free tool errors and close every pending tool call. Define how a thread with interrupted work resumes before accepting another user turn; inspect pending checkpoint work and use the appropriate LangGraph resume path. A true crash needs recovery beyond an exception handler. Test timeouts, disconnects, partial multi-tool execution and a second invocation on the same thread using a transcript-validating model stub. Keep all future submission paths outside automatic retry logic.

The selected checkpointer and `actor_id`/`thread_id` configuration otherwise match the [documented AgentCore Memory integration](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/memory-integrate-lang.html).

### F3. The thread cap is applied after an oversized fetch

**Where:** [tools.py](../../agent/src/s3mail_agent/tools.py), `run_read_thread` (lines 218–244); [jmap.py](../../agent/src/s3mail_agent/jmap.py), `Session.max_objects_in_get`; [Go Email/get](../../go/internal/jmap/email_get.go).

`read_thread` forwards every `Thread/get` member to `Email/get` through a result reference. It keeps only 30 emails after that fetch succeeds. The client discovers `maxObjectsInGet` but never uses it. The actual server advertises and enforces a 64-ID maximum.

**Reproduced:** a synthetic 65-email thread sends all 65 IDs to the dependent `Email/get`. A fake enforcing the server's actual limit returns `requestTooLarge`, which terminates the run. The normal fake does not enforce its advertised maximum, so the existing two-message thread test cannot catch this.

**Impact:** summarizing a normal long conversation fails entirely. Even below the server maximum, the implementation fetches bodies it subsequently discards.

**Fix direction:** fetch thread membership first, choose a documented bounded window using its ordered IDs, and fetch only that window in batches respecting the discovered limit. Report omissions and provide a bounded way to request earlier messages. The extra HTTP request is preferable to an invalid batched request. Test 30, 64 and 65 members, a smaller advertised limit and deletion between the two reads.

### F4. The token limit does not bound the next model request

**Where:** [graph.py](../../agent/src/s3mail_agent/graph.py), `route`, `agent` and `after_agent`; [models.py](../../agent/src/s3mail_agent/models.py), `chat_model`.

The graph checks previously reported token usage before a call and increments usage afterward. It does not reject an over-budget final answer. If the over-budget response contains tool calls, those tools execute before the next model-node check stops the run. Completion length is fixed independently of the remaining budget.

**Reproduced:** `Limits(max_tokens=500)` with a final response reporting 600 tokens returns `status: ok`, `tokens: 600`. The existing token test only covers an overrun followed by another model step.

The persistent `messages` list also grows across turns without trimming or an input-token check. Resetting the run's counters does not reset that history: a later request can resend a large conversation before any usage is charged to the new budget. Missing usage metadata is treated as zero. The step limit still bounds iterations; it does not bound request size or enforce the advertised token cap.

**Fix direction:** define whether the token cap is a hard admission limit or a usage threshold. Bound input context before each call, reserve completion capacity, reject/report overruns consistently, and handle missing usage conservatively. Define history compaction and expiry without turning old excerpts into fresh evidence. Include provider retries in cost accounting and test a long follow-up, a final-answer overrun and absent usage metadata.

### F5. The loopback callback lacks a per-login state check

**Where:** [agentcli.go](../../go/internal/agentcli/agentcli.go), `Login` and `AwaitSession` (lines 122–158 and 182–199); [identity.py](../../agent/src/s3mail_agent/identity.py), `vault_token_source` (lines 47–55).

The runtime does not send `custom_state` to Identity. The CLI accepts the first request on `/callback` with any nonempty `session_id`, then calls `CompleteResourceTokenAuth` with its AWS credentials and the constant owner ID. It does not verify that this callback belongs to the login it initiated. The existing success test supplies an arbitrary `urn:session:1` and therefore does not establish correlation or the real service's session-URI validation.

**Impact:** an unsolicited callback reaching the listener can consume the pending login and trigger a completion attempt for an unrelated URI. Login disruption is directly supported by this control flow. Cross-user token binding was not demonstrated, and the mail server's single-owner authorization remains an independent boundary.

**Fix direction:** generate a cryptographically random nonce in the CLI, pass it through the login invocation to Identity's `customState`, and verify the returned state before accepting a callback or using AWS credentials to complete binding. Reject wrong/missing state without ending the wait. Restrict the callback method and validate the URI format. Make a non-default `-return-url` agree with the URL requested by the runtime; today that flag only changes the local listener.

AWS explicitly recommends opaque callback state for CSRF protection and requires verification of the initiating user/session in its [session-binding guidance](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/oauth2-authorization-url-session-binding.html). Separately prove the chosen loopback return URL against the deployed service; that integration was not tested here.

### F6. Release promotion happens before the documented smoke test

**Where:** [agent module](../../infra/modules/agent/main.tf), `aws_bedrockagentcore_agent_runtime_endpoint.live` (lines 270–277); [root variables](../../infra/agent/variables.tf), `live_version`; [deployment procedure](../operations/deployment.md#agent-deployment).

`live_version` defaults to null, and the endpoint uses `coalesce(live_version, latest runtime version)`. Applying a new image therefore moves `live` in the same apply. Nothing in the Terraform dependency graph or checked-in agent workflow waits for a smoke test. The comments and runbook describe promotion after testing, but the default behavior does the opposite.

**Impact:** a broken image can receive owner traffic before validation. Once the agent gains write scopes, this ordering becomes more consequential.

**Fix direction:** keep the currently approved version pinned while creating a candidate, invoke and validate that candidate, then explicitly promote its version. Document the first-deploy exception and rollback path. Add a plan assertion that a candidate-only release leaves `live` unchanged. The acknowledged missing evaluation/CI deployment machinery remains an open A0 gate, not evidence that promotion is safe.

### F7. Runtime log retention is left to service defaults

**Where:** [agent module](../../infra/modules/agent/main.tf), `OwnLogGroups`/`WriteOwnLogs` and runtime environment; [permission inventory](../operations/permissions.md#s3mail-agent-runtime-agentcore-runtime-execution-role).

The role can create runtime log groups, but neither the module nor the root declares a CloudWatch log group or retention policy. The six module tests do not check retention. Consequently the repository does not enforce bounded retention for the agent's logs. CloudWatch's default is indefinite retention, as documented in [log-group management](https://docs.aws.amazon.com/AmazonCloudWatch/latest/logs/Working-with-log-groups-and-streams.html).

**Impact:** invocation and dependency logs can accumulate indefinitely. Disabling framework content capture is valuable but does not substitute for retention or a scan of actual exported telemetry, including exception events.

**Fix direction:** manage the runtime's actual log groups and set an explicit retention period, consistent with the project's monitoring policy. If service-generated names require post-creation adoption, make that part of provisioning and drift checks. Add a Terraform assertion and verify real log/span output with synthetic canaries. This review did not inspect deployed log settings or establish a content leak.

### F8. Untrusted answers are written directly to the terminal

**Where:** [agent CLI](../../go/cmd/agent/main.go), `ask`, line 140 (`fmt.Println(res.Answer)`).

The model's answer may quote sender-controlled content, but the CLI sends it to stdout without filtering terminal escape/control sequences. An answer containing ESC/OSC sequences is interpreted by a capable terminal rather than displayed purely as text. JSON escaping during transport does not help after decoding.

**Impact:** a malicious or reflected answer can alter the visible terminal output; some terminals support additional actions such as clipboard updates. The available effects depend on the terminal. No live model exploit or terminal action was attempted in this review.

**Fix direction:** apply a plain-text terminal output policy at the CLI boundary, preserving ordinary Unicode, newlines and tabs while neutralizing terminal controls. Test synthetic CSI, OSC, BEL and carriage-return output. Keep this display policy in the [client specification](../specs/clients.md#design), with the agent spec linking to it.

## Design points to resolve before later phases

- **A2 unapproved writes conflict with the invariant (Low).** The invariants allow only Drafts writes without approval, but `import_digest` creates mail in the `Agent` mailbox without approval. State an explicit digest exception, or require approval; update the invariant and A2 checks together in the owning agent spec.
- **A3 recovery must distinguish an attempted submission from proof of acceptance.** The conditional durable marker is the right starting point. Preserve the rule that an empty reconciliation query after an ambiguous result cannot authorize another automatic create. Test response loss, a crash after marker creation but before submission, and a crash after successful creation. These are design acceptance needs; A3 has no implementation to review yet.
- **Approval objects need a precise serialization contract.** Define canonical hashing, account/mode binding, expiry checks, and which exact arguments the owner sees, including envelope and success hooks. Do not infer a draft-content race from a stable Email ID: the existing JMAP implementation rejects content mutations under that ID. Review these details when the A3 executor exists.
- **Keep the accepted external model boundary explicit.** Mail excerpts intentionally leave AWS. Preserve per-request provider privacy controls, the separate OpenRouter credit limit and content-free instrumentation when changing models or dependencies. Account privacy settings and actual provider routing were not inspected here.
- **Reconcile stale overview status text.** The documentation index, architecture and diagram maintenance guide still contain design-only/no-deployment statements, while the agent spec and roadmap record implementation and deployment. Update those summaries without claiming any unproven gate passed.

## Validation results and remaining evidence

| Check | Result |
| --- | --- |
| `make agent-check` | Passed: Ruff lint/format, strict mypy, 42 Python tests; one upstream Pydantic deprecation warning |
| `cd go && go test ./internal/agentcli ./cmd/agent` | Passed; CLI main package has no tests |
| `terraform -chdir=infra/modules/agent test` | Passed: six mocked tests, no AWS resources created |
| Temporary synthetic probes | Reproduced F1–F4; used `InMemorySaver`, scripted model and HTTP mock transports |
| Documentation links and whitespace | Checked for this report and its index entry |

The Python and Go checks needed access to existing caches outside the workspace. The Terraform module was initialized from the already-installed provider, and its mocked tests needed the provider plugin to run outside the sandbox. These are local tooling constraints, not application failures.

The full repository `make check` was not run: this change only adds review documentation, and the checks above target the reviewed agent. No commit or push was made. Temporary probe code is not part of the repository test suite.

The [A0 gate](../specs/agent.md#a0-gate-mvp) remains open. In particular, there is no result here for deployed consent and refresh, write-scope denial with a real A0 token, absence of live secrets after Terraform refresh, the 50-question/30-injection evaluations, an exported telemetry scan, latency/cost baselines, CI apply or the kill-switch rehearsal. The existing five-email fake and scripted responses establish deterministic guard behavior, not real model injection resistance or answer quality. IAM mock assertions do not establish effective deployed permissions; the specified project permissions boundary is also still deferred in bootstrap.

**Diagram impact:** none. This report proposes follow-up work but changes no component, integration, data flow, authorization boundary or deployment stage; the embedded AWS diagram is unchanged.

## Resolution

Added 2026-10-18 after the review; the findings above are unchanged.

| ID | Resolution | Tests |
| --- | --- | --- |
| F1 | Every answer needs a checked [evidence marker](../specs/agent.md#answer-evidence) (`[email:<id>]`, `[mailboxes]`, `[searched]` or `[no-mail]`); missing, malformed and unbacked markers get one retry, then `invalid_citations`. `[no-mail]` answers are labelled by the CLI. | `agent/tests/test_graph.py`: missing, malformed, unbacked and stale-follow-up cases |
| F2 | Transport errors and invalid JSON become retried `JmapError`s; any tool exception closes every pending call. The model sees only earlier questions and accepted answers, so interrupted checkpoints never reach it. | Timeout, retry and interrupted-checkpoint tests; the scripted model now rejects unanswered tool calls |
| F3 | `read_thread` reads membership, then the newest 30 emails (or earlier ones with `skip_latest`) in batches within `maxObjectsInGet`; search limits are clamped. | 30/64/65-member threads, a smaller server limit, deletion between reads; the fake server enforces its limit |
| F4 | Token admission before each call (estimate plus completion cap), overrun ends the run before its tool calls or answer, missing usage is charged at the estimate, and context is bounded to ten earlier turns. | Overrun, admission, missing-usage and context-window tests |
| F5 | `agent login` sends a 256-bit state passed to the vault as `customState`, listens on the runtime's own return URL, and accepts only a GET with that state and a well-formed session URI. `-return-url` is removed. | `go/internal/agentcli`: forged, missing and wrong-method callbacks |
| F6 | `live` serves only `live_version`, which Terraform requires with an image; new versions are candidates on `DEFAULT` until promoted. | `infra/modules/agent/tests`: a new version does not move `live` |
| F7 | Terraform owns both runtime log groups with 30-day retention; existing groups are imported. | Retention assertion in the module tests |
| F8 | The CLI prints answers through `PlainText`, dropping C0/C1 controls, DEL and bidirectional overrides; the policy is in the [client spec](../specs/clients.md#design). | `TestPlainTextDropsTerminalControls` |

Design points: the digest import is now an explicit invariant exception, and the overview status text no longer says nothing is implemented. A3 recovery and approval serialization remain open for that phase.
