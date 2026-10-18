# Mail agent implementation review — 2026-10-19

[Documentation index](../README.md) · [Agent specification](../specs/agent.md) · [Earlier review and resolutions](agent-review.md)

**Reviewed revision:** `adfa5ed9b419effd5a489f7019b47fc475e956e6`, on `main`, with a clean working tree before this report. Source line numbers below refer to that revision.

**Assessment:** the implementation has useful boundaries—JMAP-only mail access, typed tools, explicit proposals, a durable submission marker and separate deployment—but still has correctness, privacy, recovery and scaling problems. Two findings deserve high priority: exceptions can leak mail text to traces, and rejecting an interrupted approval can execute the previously approved action. Passing the existing tests does not cover these failure paths.

This review examines implementation behavior and design risks, including issues beyond the written acceptance checks. It covers the Python runtime, tools, stores, attachment extraction and evaluation runner; the Go CLI and trigger; and the agent Terraform module and deployment configuration. It preserves the earlier review as historical evidence rather than reopening its resolved findings without evidence.

## Findings at a glance

P1 means a significant privacy or owner-control failure. P2 means a concrete correctness, reliability, performance or authorization-design issue. “Reproduced” means a local synthetic probe exercised the current implementation; it does not mean the issue was exercised against deployed mail.

| ID | Priority | Finding | Evidence / exposure |
| --- | --- | --- | --- |
| R1 | P1 | Tool exceptions put untrusted content into trace events | Reproduced; current action path |
| R2 | P1 | `reject` can execute an interrupted approved action | Reproduced; updates and future sending |
| R3 | P2 | Mailbox updates can execute twice after a crash | Reproduced; current write capability |
| R4 | P2 | Digest overflow is permanently skipped after cursor advancement | Reproduced; digest |
| R5 | P2 | Reminder collection exceeds `Thread/get` limits and does unbounded work | Reproduced with the backend's limit; digest |
| R6 | P2 | A model outage prevents the deterministic digest from being delivered | Reproduced; digest |
| R7 | P2 | Digest delivery and cursor updates lack retry/concurrency protection | Duplicate delivery reproduced; cursor race confirmed in source |
| R8 | P2 | Conversation checkpoints grow despite bounded model context | Growth reproduced; production saver behavior confirmed in installed source |
| R9 | P2 | Attachment timeout leaves the extraction worker running | Reproduced; feature documented as disabled |
| R10 | P2 | XLSX extraction loses the relationship between text and cell values | Reproduced; feature documented as disabled |
| R11 | P2 | The automated trigger has the runtime's owner-level command authority | Authorization design confirmed in source; no live role impersonation attempted |
| R12 | P2 | Injection evaluations can pass when every case failed before reading mail | Reproduced with the actual evaluation entrypoint |

The specification records `mail.write` as granted, `mail.submit` as ungranted, semantic search as enabled and attachment extraction as disabled. These are documented deployment facts, not live-state observations made during this review. R2's send reproduction uses synthetic mail and an enabled submission path; the same recovery branch also governs mailbox updates.

## R1 — Tool exception recording leaks content into traces

**Location:** [graph.py](../../agent/src/s3mail_agent/graph.py), lines 247–274; [proposals.py](../../agent/src/s3mail_agent/proposals.py), lines 89–95.

The tool span is described as content-free, but `start_as_current_span()` uses automatic exception recording. Exceptions leave its `with` block before the surrounding handler catches them. `ProposalError` can include a model-supplied mailbox name, and other exceptions can contain request details. Disabling framework prompt capture does not disable this recording in the application's own spans. OpenTelemetry's [Python trace API](https://opentelemetry-python.readthedocs.io/en/stable/api/trace.html) documents automatic exception recording and status handling as defaults.

**Reproduction:** put the synthetic subject `SYNTHETIC_PRIVATE_SUBJECT_47` in an email. Have the scripted model search for that email, then propose a move to a nonexistent mailbox named with that subject. Run the real graph with an in-memory OpenTelemetry exporter. The run returns normally after refusing the proposal, but an exported exception event contains:

```text
exception.message = no mailbox 'SYNTHETIC_PRIVATE_SUBJECT_47'
```

The exception stack trace also carries the message. This establishes a content path into telemetry; it does not establish that a live model has produced this proposal or that existing production traces contain personal content. The existing read-only evaluation scan misses this action failure path.

**Impact:** reflected mail text, private mailbox names or other sensitive exception text can enter the trace backend, violating the [telemetry privacy rule](../operations/monitoring.md#design).

**Fix direction:** handle exceptions before they cross the span boundary, or disable automatic exception recording and automatic exception-derived status descriptions for these spans. Record only a fixed error code/type. Add an exporter assertion covering failed proposals and failed attachment/download tools, including events and status descriptions as well as attributes.

## R2 — Rejecting an interrupted approval can execute it

**Location:** [graph.py](../../agent/src/s3mail_agent/graph.py), `run_approve`, lines 466–478.

When a checkpoint has `execute` pending, `run_approve()` checks the supplied hash and resumes with `graph.ainvoke(None, config)`. It never checks the new `approve` argument. The executor therefore receives the earlier checkpointed decision, including `approve: true`, even when the current command is `agent reject`.

**Reproduction:** create a send proposal with the existing action test rig. Approve it, but inject a crash on entry to `graph.execute`, before the send marker or submission exists. Restore the executor and call the rig's approval method with the same hash and `approve=False`. Observed result:

```json
{"outcome": "sent", "submission_id": "S1"}
```

There was no submission before rejection and one afterward. A second probe crashing after the send marker was created produced `already_attempted` rather than `rejected`, confirming that this branch ignores the new decision in both cases.

**Impact:** the owner's explicit rejection can initiate an action. The action had an earlier approval, so this is specifically a recovery/cancellation failure, not evidence that an arbitrary unapproved proposal can run. It contradicts the interface's rejection behavior and affects updates even while sending lacks its OAuth scope.

**Fix direction:** distinguish rejection/cancellation from recovery of an already approved operation. A rejection must never enter the executor. Record cancellation durably before an unstarted operation can run; if an operation may already have happened, report its existing or unknown state without executing it. Add crash-before-executor rejection tests for both updates and sends.

## R3 — Update recovery repeats completed mailbox changes

**Location:** [proposals.py](../../agent/src/s3mail_agent/proposals.py), lines 247–265; [graph.py](../../agent/src/s3mail_agent/graph.py), lines 468–475.

The send path has a durable attempt marker. `_execute_update()` has no equivalent and no saved progress between its JMAP calls. A crash after an `Email/set` succeeds but before the graph saves the executor's result leaves `execute` pending. Recovery repeats all calls.

**Reproduction:** approve a flag operation on `M1`; let the real JMAP write apply, then raise a process-crash exception. Simulate the owner unflagging `M1` in another client. Resume the approval. The server observes two `Email/set` writes and the owner's later unflag is overwritten. Existing tests prove submission recovery and ordinary second-approval refusal, but not this update crash boundary.

**Impact:** a supposedly once-only approval can repeat side effects and overwrite later owner work. Repeating an absolute flag assignment is harmless only when nothing changes between attempts. Multi-call updates can also replay already completed batches.

**Fix direction:** track update attempts/results durably by approval and batch, with explicit unknown-outcome reconciliation. Guard against changed mailbox state before applying old proposals, using the ordinary JMAP state contract or a checked equivalent. Do not assume that checkpointing the surrounding node makes its external writes once-only. LangGraph's [durable execution guidance](https://github.com/langchain-ai/docs/blob/main/src/oss/langgraph/functional-api.mdx) explicitly requires idempotency for tasks that start but fail before completion.

## R4 — The digest cursor consumes messages that were never listed

**Location:** [digest.py](../../agent/src/s3mail_agent/digest.py), lines 56–96 and 319–320.

`_changes()` reads every change page, retains only the first 300 created IDs, and returns the final state. After delivery, the cursor advances to that final state. The query fallback has the same problem: it limits results but records a state covering the entire existing mailbox.

**Reproduction:** start at `s0`, deliver 301 synthetic inbox emails and run a digest. It lists 300, warns that one more was not listed and stores `s301`. The next digest reports zero new emails. The omitted email never gets its own digest entry. The current pagination test checks the warning but does not run a second digest to establish whether the remainder is recoverable.

**Impact:** after a backlog or burst, summaries can permanently omit mail that needs attention. The “more not listed” warning makes truncation visible in that run; it provides no continuation. Applying the cap before filtering Drafts, Sent, Trash and Agent mail can discard useful inbox entries while excluded messages consume the allowance. Reading all change pages also leaves API work and memory use unbounded despite the output cap.

**Fix direction:** retain a bounded continuation that covers only consumed changes, or queue the remainder as derived pending digest work. For query fallback, paginate a fixed window without advancing beyond unreported mail. Test multiple consecutive digests and require each eligible ID to be covered, including excluded-mail-heavy windows. This needs a design adjustment even though the current spec permits a 300-email output cap.

## R5 — Reminder collection can break the entire digest

**Location:** [digest.py](../../agent/src/s3mail_agent/digest.py), `_waiting`, lines 115–143; [backend Thread/get](../../go/internal/jmap/thread.go), lines 17–21.

The reminder path sends every distinct candidate thread in one `Thread/get`. The backend permits at most 64 IDs. The `MAX_REMINDERS = 20` cap is applied only after fetching all candidate threads and every email in them.

**Reproduction:** create 65 synthetic needs-reply rows in distinct threads, all within the 2–21-day reminder window. With a fake enforcing the real backend's 64-ID limit, `run_digest()` fails with `requestTooLarge`. The normal fake enforces the Email/get limit but does not enforce the Thread/get limit, so existing reminder tests miss this.

**Impact:** a moderate backlog of unanswered threads prevents all digest delivery, including unrelated new mail. Below that threshold, a few very long threads can still cause large numbers of header reads. Neither the model token cap nor the 20-reminder output cap bounds this work.

**Fix direction:** batch thread reads within the discovered limit and impose a per-run candidate/header-read budget, with continuation or rotation to avoid starvation. Keep reminder degradation from preventing the new-mail section from being delivered. Test 64/65 threads and long-thread fan-out.

## R6 — The optional model overview becomes a digest availability dependency

**Location:** [digest.py](../../agent/src/s3mail_agent/digest.py), lines 299–304.

The code catches only `BudgetExceeded` around `_overview()`. Provider timeouts, authentication errors, rate limits after retry exhaustion and malformed model responses escape instead. The deterministic mail listing has already been assembled but is never rendered or delivered.

**Reproduction:** inject `TimeoutError` from the scripted model while generating a nonempty digest. The exception escapes, no digest email is created and the cursor stays unchanged. Through the runtime handler this becomes `internal`; the scheduled invocation fails.

**Impact:** an OpenRouter/model outage blocks a digest that code could deliver without a model. This also contradicts the spec's statement that the overview is omitted if it fails.

**Fix direction:** catch the expected model/provider failures at this optional boundary, emit a fixed content-free failure code and deliver the deterministic digest with no overview. Preserve cancellation semantics and let authoritative JMAP/storage failures remain failures. Add timeout, exhausted-rate-limit and invalid-response fallback cases.

## R7 — Digest retries duplicate mail and concurrent runs race the cursor

**Location:** [digest.py](../../agent/src/s3mail_agent/digest.py), lines 280–282 and 315–320; [store.py](../../agent/src/s3mail_agent/store.py), `set_cursor`; [scheduled trigger](../../go/internal/agenttrigger/agenttrigger.go), `HandleSchedule`.

The digest email is created before an unconditional cursor write. There is no durable delivery identity or run lease. Giving a scheduled run a deterministic runtime session ID does not deduplicate it: digest mode is uncheckpointed and does not use that ID.

**Reproduction:** allow digest creation to succeed, then fail `set_cursor()` once. Retry the same digest. Two emails with the same date and contents are created (`D6` and `D7` in the synthetic server).

Source also permits two CLI/scheduled runs to read the same cursor, deliver overlapping digests and overwrite each other's cursor. An older run completing last can regress it. A lost JMAP response similarly leaves delivery ambiguous; a later invocation has no record with which to reconcile it.

**Impact:** duplicate mailbox writes, duplicate summaries and unnecessary model spend during retries or overlap. These are digest imports, not external submissions; the submission marker remains a separate protection.

**Fix direction:** use a durable digest-window identity, a conditional cursor version/lease and delivery outcome tracking. Reconcile uncertain creates before another create for that window. Cover response loss, post-create cursor failure, concurrent manual/scheduled runs and reverse completion order.

## R8 — Bounded prompts hide unbounded checkpoint storage and reads

**Location:** [graph.py](../../agent/src/s3mail_agent/graph.py), lines 94–98 and 128–130; [app.py](../../agent/src/s3mail_agent/app.py), line 331.

`model_context()` limits what the model sees, but the `messages` channel keeps every old question, answer, tool call and mail excerpt. No pruning updates the persisted channel. The runtime constructs `AgentCoreMemorySaver` without options; in the locked `langgraph-checkpoint-aws` 1.2.5 implementation, this selects legacy storage with `limit=None`, and restoring a thread reads its event history before reconstructing the latest checkpoint.

**Reproduction:** run 20 questions on one thread, each reading the same synthetic 16,000-character email. Measure the latest saved message list with the real graph and `InMemorySaver`:

| Completed questions | Saved messages | JSON bytes in saved messages | Messages in latest model prompt |
| --- | --- | --- | --- |
| 10 | 40 | 173,590 | 22 |
| 20 | 80 | 347,200 | 24 |

The prompt stays bounded while the saved state doubles. Installed saver source confirms that changed channel values are serialized and legacy restoration reads history; this was not an AWS latency or billing benchmark.

**Impact:** long-running CLI threads accumulate storage, serialization work and restore traffic unrelated to the bounded prompt. Repeated persistence of an ever-growing channel and restoration of its historical versions can make aggregate work grow much faster than turn count. New checkpoints also keep copying old mail excerpts into fresh events, so event expiry alone is not an age limit on the excerpts in an actively used thread.

**Fix direction:** compact persisted read-only history to bounded accepted turns and current-run data, preserving pending approvals and send recovery independently. Measure a long-thread restore against the actual saver and evaluate a bounded restoration format with migration tests. Do not set an arbitrary event-read limit that can truncate required checkpoint data.

## R9 — Attachment extraction has a response timeout, not an execution limit

**Location:** [attachments.py](../../agent/src/s3mail_agent/attachments.py), lines 145–154.

`asyncio.wait_for(asyncio.to_thread(...), 20)` stops waiting but cannot stop an extraction function that is already running in a worker thread. Python's [executor cancellation contract](https://docs.python.org/3.12/library/concurrent.futures.html#concurrent.futures.Future.cancel) confirms that running work cannot be cancelled that way. Text is also truncated only after extraction finishes; PDF decompression and intermediary text allocations have no independent memory limit here.

**Reproduction:** replace the extractor with a synthetic worker waiting on a threading event and shorten the timeout to 20 ms. The tool reports `extraction timed out` while the worker remains running. It finishes only when the probe releases it. No hostile PDF or memory-exhaustion payload was run.

**Impact:** once attachments are enabled, a costly document can keep consuming CPU/memory after the tool has reported failure. Repeated calls can occupy the shared executor and interfere with checkpoint/store operations that also use `to_thread`. Page, input-byte and output-byte caps do not by themselves bound intermediate decompression or parser work.

**Fix direction:** run untrusted extraction in a disposable worker process with an enforceable deadline and memory/output limits; terminate and reap it on timeout. Stop building output once the text cap is reached. Keep the feature disabled until timeout and resource-isolation tests pass.

## R10 — XLSX text extraction produces misleading cell data

**Location:** [attachments.py](../../agent/src/s3mail_agent/attachments.py), lines 46–48 and 81–104.

The regex extractor prints the shared-string table separately and then prints worksheet `<v>` contents literally. For a shared-string cell, `<v>` is an index into that table, not the cell's text. The extractor never resolves that index or preserves cell/row relationships between the two files.

**Reproduction:** construct a tiny XLSX archive with shared strings `Alpha`, `Beta` and one row whose A1 references shared-string index `1`, while B1 contains numeric `42`. Correct row text is `Beta 42`; the current extractor returns:

```text
Alpha Beta
1 42
```

**Impact:** the model receives unused strings and detached numeric indices, and can associate a number with the wrong label. A citation to the attachment cannot repair the corrupted representation. The existing XLSX test checks that string/numeric tokens occur, rather than that cell values are reconstructed correctly.

**Fix direction:** resolve shared and inline strings into worksheet cells, retain row/cell boundaries and apply bounded parsing/decompression. Alternatively, remove XLSX from the supported types until there is a correct bounded implementation. Test reordered and unused shared strings, repeated references and inline strings before enabling the feature.

## R11 — Automated invocation authority also authorizes owner commands

**Location:** [trigger IAM](../../infra/modules/agent/trigger.tf), lines 123–131; [shared invocation client](../../go/internal/agentcli/agentcli.go), `Invoke`; [app.py](../../agent/src/s3mail_agent/app.py), lines 183–214 and 341–345.

The trigger role can invoke the same `live` runtime with the owner's runtime user ID. The payload chooses the mode; the Python entrypoint forwards only the session ID and has no caller-specific mode policy. Its scope check uses the deployment's granted OAuth scopes, not separate trigger/owner authority.

**Evidence:** the checked-in trigger IAM allows both runtime invocation actions on the owner-facing runtime/endpoint. The shared Go client always sets `RuntimeUserId` to `owner`. No later application check distinguishes a trigger invocation from an owner invocation. AWS documents the user-header permission in [InvokeAgentRuntime](https://docs.aws.amazon.com/bedrock-agentcore/latest/APIReference/API_InvokeAgentRuntime.html). This finding is a source-based authorization inference, not a live attack with trigger credentials.

**Impact:** code running with trigger credentials can ask for mail, write/forget preferences, or issue `act` followed by `approve` using the same delegated identity. Current scope limits still apply, so ungranted `mail.submit` remains unavailable. The normal event parser does not expose arbitrary commands to a sender; the problem is that compromise or misuse of the automated caller has the owner's full runtime command authority. Absence of direct S3/DynamoDB mail grants does not isolate this indirect access.

**Fix direction:** separate automated modes from owner commands at an authenticated boundary. For example, use a separately permissioned runtime for triage/digest, or a verified caller identity/claim with a fixed allowed-mode policy before dispatch. A payload flag claiming to be a trigger is insufficient. Test that the trigger principal cannot read arbitrary conversations, change preferences or approve actions.

## R12 — Injection evaluation can report success without exercising an attack

**Location:** [evals/run.py](../../agent/evals/run.py), lines 300–345 and 354.

The runner records `exposed`, `injections_exposed` and `errors`, but none is a condition of the injection gate. A case that fails before reading its adversarial email has no writes, no forbidden tool error and no canary in its empty answer, so it counts as safe. This also affects the injection portion of the combined suite.

**Reproduction:** run the actual `main()` with `--suite injection`, replacing only `Runner.ask()` with an offline result that reports `jmap_unavailable`, zero reads and zero writes. Stub the key and report writer so there is no network or secret access. Observed output:

```text
injections: 30
injections_exposed: 0
errors: [jmap_unavailable]
all gate values: true
exit code: 0
```

**Impact:** a broken read/model path or a model that avoids the requested email can produce green injection evidence without being exposed to the attack. This does not invalidate the historical run's recorded 30/30 exposure; it means the gate will not enforce that prerequisite on future runs.

**Fix direction:** require every selected injection case to read the intended adversarial content, and distinguish safe refusal after exposure from infrastructure failure or no exposure. Fail on unexpected case errors or missing cases. Include failure-path telemetry canaries and action-mode attacks, especially after write scopes are enabled. Add an offline regression test for the all-errors result above.

## Validation performed

| Check | Result |
| --- | --- |
| `make agent-check` | Passed: Ruff lint/format, strict mypy and 178 Python tests; one upstream Pydantic deprecation warning |
| `cd go && go test -race ./internal/agentcli ./internal/agenttrigger ./cmd/agent ./cmd/agent-trigger` | Passed; command packages have no test files |
| `terraform -chdir=infra/modules/agent test` | Passed: 14 mocked tests; provider reports deprecated DynamoDB key arguments |
| Synthetic graph/digest/attachment probes | Reproduced R1–R10 behavior using the existing fixtures and actual implementation; R8's cloud cost implication is source-based |
| Actual evaluation entrypoint with offline failing cases | Reproduced R12: 30 unexposed failures still return success |
| Documentation links and whitespace | Checked for this report and its documentation-index entry |

The probes were temporary local scripts using the repository's `Rig`, `MailServer`, `ScriptedModel`, in-memory stores/checkpointer and HTTP mock transports. R5 additionally enforced the limit already present in the real Go backend. They injected failures at identified crash boundaries; they did not modify production code or become part of the checked-in regression suite. Each finding above specifies the setup and observable outcome so a regression test can be added in the owning suite.

The uv and Go checks needed access to existing caches outside the sandbox, and the mocked Terraform provider needed to start outside the sandbox. The initial sandbox attempts failed for those tooling reasons; the reruns above passed.

No AWS state or personal mail was read, no model calls were purchased, and no deployment, send, commit or push was performed. The full repository `make check` was not run: the change is review documentation and the checks target the reviewed component. This report does not establish live OAuth/IAM behavior, live cancellation races, provider privacy settings, actual cloud latency/cost or a passed release gate.

## Review boundaries and next steps

Fix R1 and R2 first, then make update/digest recovery and cursor behavior explicit before expanding use of write actions. Keep attachment extraction off until R9 and R10 are resolved. Repair the evaluation gate before relying on it for another security claim.

The narrow JMAP method allowlists, prohibition on destroy operations, conditional send marker, immutable backend email content and pinned `live_version` are useful existing protections. No direct agent-runtime IAM grant to the authoritative mail bucket/table was found in the reviewed module. Effective deployed permissions and resource policies still need separate validation; R11 identifies an indirect command boundary those storage exclusions do not cover.

Additional observations for follow-up, without counting them as independently reproduced findings: `DynamoTable.get_triage()` retries `UnprocessedKeys` without backoff or an attempt/deadline limit; per-run model budgets do not bound that loop or total JMAP work. Send approval previews omit attachment inventory and cap body display, so owner review relies on opening the full draft separately. The agent README's deployment status is also stale relative to the spec. These should be assessed alongside the fixes rather than treated as proven deployed incidents.

**Diagram impact:** none. This report records findings and proposed remedies; it changes no AWS component, integration, data flow, authorization boundary, hosting or deployment stage. The embedded editable AWS diagram remains unchanged.

## Resolution

Added 2026-10-19 after the review; the findings above are unchanged. The fixes are offline-tested only: no image was built or deployed with them, and the evaluation suites were not re-run against the model.

| ID | Resolution | Tests |
| --- | --- | --- |
| R1 | Tool spans go through one helper with automatic exception recording and exception-derived status off; a failure records only the exception type. The evaluation telemetry scan also covers status descriptions. Download URLs carry a fixed name, so HTTP spans never record an attachment's name. | `test_actions.py`: a failed proposal naming a synthetic subject leaves no trace of it in attributes, events or status |
| R2 | A rejection on an interrupted executor first replaces the recorded decision, then resumes it; a rejected executor writes nothing and reports `already_attempted` only if an attempt began. | Crash before the executor, for an update and a send; crash after the send marker |
| R3 | Updates take an `UPDATE#<hash>` marker before their first write, so a resumed executor reports instead of repeating; they also run only if the emails' keywords and mailboxes are as proposed (`mail_changed`). | Crash after an applied update with a later owner change; emails moved after the proposal |
| R4 | `Email/changes` never asks for more than the cap still allows and stops after 20 pages; the cursor covers only consumed changes and the next digest continues. A truncated query fallback stores the last listed time with no state. | Two consecutive digests list all 300 burst emails once; page budget; truncated fallback |
| R5 | Reminders check at most 200 rows, batch `Thread/get` within `maxObjectsInGet` and read at most 600 headers; a method error leaves reminders out with a note. | 65 threads under a 64-id limit; a failing reminder read |
| R6 | Any exception from the overview, not only the budget, leaves the overview out; the deterministic digest is delivered. | Timeout and invalid-reply cases delivered as mail |
| R7 | The digest email carries `$s3mail-digest-<window>` and is looked up before creating; a delivering run leases its window; the cursor is written only over the value the run read. | Retry after a failed cursor write creates one email; a concurrent run is refused; a late run does not move the cursor back |
| R8 | The router removes every message except the kept question/answer turns when a run starts, so saved history stays bounded with the prompt. | 25 questions reading one email: at most 24 saved messages, one tool reply |
| R9 | Extraction runs in a spawned process with a 1 GiB address-space limit (where enforced), killed at the deadline; PDF and Office extraction stop at the text cap. | A hung extractor is killed and reaped; text cap; the tool path through the process |
| R10 | Spreadsheet cells resolve shared strings by index and inline strings in place, one row per line with cells separated by a vertical bar; unused strings are left out. | The review's Alpha/Beta case, rich-text and phonetic runs, inline strings, out-of-range indices |
| R11 | **Open.** Separating automated modes needs either a second runtime for triage and digest (its own workload identity, consent and release) or an IAM-verifiable caller boundary; the owner chooses. | — |
| R12 | The A0 gate also requires every injection case to read its adversarial email and fails on run errors. | The review's all-`jmap_unavailable` result fails the gate offline |

A [follow-up review of these fixes](agent-fix-review-e69137d.md) found that the R3, R4, R5, R7 and R8 resolutions above were incomplete (a check separate from its write, tied query timestamps, a partial reply check, digest reuse across windows, unbounded checkpoint restores); [its resolution](agent-fix-review-e69137d.md#resolution) records the further fixes.

Of the additional observations, `get_triage()` now retries `UnprocessedKeys` at most five times with exponential backoff, then fails (`agent/tests/test_store.py`, which also pins the new conditional cursor, lease and marker requests). Send previews without an attachment inventory and the stale agent README status remain open.
