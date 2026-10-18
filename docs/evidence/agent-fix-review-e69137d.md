# Review of agent fixes in e69137d

[Documentation index](../README.md) · [Original findings](agent-implementation-review-2026-10-19.md)

**Reviewed:** `e69137d9f478196c122cdca528d753bf683dfb81`, “Fix the agent implementation review findings except R11”, dated 2026-10-19. This commit is on `worktree-agent-review-fixes`; local and fetched `origin/main` still point to `adfa5ed`. Review and tests used an isolated export of the exact commit. Source line numbers below refer to `e69137d`.

**Verdict: no, it does not resolve all the issues.** The original two high-priority cases are fixed, and the commit adds useful regression coverage. R11 is explicitly left open. R4, R7 and R8 remain partially resolved; the R3 and R5 changes also leave a concurrency gap and introduce a reminder regression. The resolution table in the commit overstates completion in those areas.

## Remaining findings

### C1 — P2: Retrying a digest can advance past new mail absent from the delivered email

**Related finding:** R7. **Locations:** [digest.py](../../agent/src/s3mail_agent/digest.py), lines 307–322, 349–355 and 399–406.

The delivery key identifies only the starting cursor. On a retry, `_changes()` computes a new ending state, potentially covering mail that arrived after the first attempt. `deliver_digest()` finds the earlier digest by that starting key and returns it unchanged. `run_digest()` then advances the cursor to the newly computed ending state, even though the returned digest does not contain the newly arrived mail.

**Reproduced against this commit:**

1. Deliver a digest successfully, then fail its cursor write, as in the new `FlakyCursor` test.
2. Deliver a new synthetic message, `NEW`, before retrying.
3. Retry after the ten-minute lease expires.
4. The result reports four new emails but returns the first attempt's digest `D6`, whose body has no `[email:NEW]` entry.
5. The next digest reports zero new emails. `NEW` has been consumed by the cursor without being listed.

The same failure needs no injected error when using `deliver="mail", peek=True`: deliver a preview, receive another message, then deliver normally. The normal run reuses the preview email and advances past the new message. The existing retry test adds no incoming mail between attempts, so it misses this case.

**Required change:** persist the delivery's actual end cursor/window before creating its email and reconcile that exact window on retry. Reusing an existing email must advance only to the state represented by that email. Preview delivery must not consume the normal delivery identity. Add tests with mail arriving between attempts and between preview and delivery.

This skips digest coverage; it does not delete the underlying mailbox message.

### C2 — P2: Query fallback still discards overflow when timestamps tie

**Related finding:** R4. **Location:** [digest.py](../../agent/src/s3mail_agent/digest.py), lines 124–135.

The `Email/changes` continuation is improved, but query fallback has only a timestamp as its continuation. If more than 300 messages share that timestamp, the second query repeats the same page. When `at == after`, the code explicitly gives up and saves the current complete Email state, discarding the remainder from future digests.

**Reproduced:** start with an expired change state and 301 synthetic messages at the same `receivedAt`. Three consecutive digests report `300`, `300`, `0` emails. The first 300 repeat, the last message never appears, and the stored state becomes `s301`. The backend's `after` filter includes equality, matching the reproduction.

**Required change:** use a stable continuation that distinguishes messages sharing a timestamp, such as a bounded query snapshot/ID continuation. Do not advance to a complete state when unlisted results remain. The new fallback test checks only the first run; extend it through completion with timestamp ties. Also make the fake honor the requested sort direction: it currently sorts query results newest-first regardless of `isAscending`, weakening this coverage.

### C3 — P2: The new stale-update check is separate from the write

**Related finding:** R3. **Location:** [proposals.py](../../agent/src/s3mail_agent/proposals.py), lines 291–302.

The new update marker prevents the original crash replay, which is a real fix. However, the new promise that an old approval cannot act over a later owner change is not enforced atomically. The executor reads and compares mailbox/keyword state, then sends an unconditional `Email/set`. Another client can change the email between those requests.

**Reproduced:** propose moving `M1` from Inbox to Archive. Allow the executor's current-state check to pass, then simulate the owner moving `M1` to Trash immediately before the write. The executor reports `done`; the resulting mailboxes are both Trash and Archive. It neither returns `mail_changed` nor produces the approved Archive-only result.

**Required change:** tie the write to the validated state using the ordinary JMAP `ifInState` contract or an equivalent atomic precondition. Handle state changes between update batches explicitly. Test an intervening change after the validation read, not only one made before the executor starts.

### C4 — P2: Limiting reminder reads hides an existing sent reply

**Related finding:** R5; regression introduced by its fix. **Location:** [digest.py](../../agent/src/s3mail_agent/digest.py), lines 169–187.

The executor now reads only each thread's last 20 emails, plus the original needs-reply message. It treats the absence of a sent reply in that partial sample as evidence that the thread has not been answered. A sent reply outside the last 20 members is invisible to that check.

**Reproduced:** create an incoming needs-reply message, a later owner reply in Sent, then 21 more incoming messages in the same thread. Keep the original message without `$answered`, which the Sent fallback is designed to handle. The digest produces one “Still waiting on you” reminder for the already answered original. Its header reads never include the existing sent reply.

**Required change:** use an authoritative bounded query for sent messages in the thread after the original, or treat incomplete inspection as unknown and omit the reminder. A partial thread sample cannot prove that no sent reply exists. Add a long-thread case with the sent reply outside the sampled tail.

The 65-thread protocol failure is fixed, but the database-work limit is also incomplete: `_waiting()` slices to 200 candidates only after [store.py](../../agent/src/s3mail_agent/store.py), lines 209–221, has paginated and materialized every matching GSI row. Push the candidate limit and newest-first ordering into the store query, with a bounded continuation policy.

### C5 — P2: Checkpoint restoration still reads unbounded legacy history

**Related finding:** R8. **Location:** [app.py](../../agent/src/s3mail_agent/app.py), line 331; [graph.py](../../agent/src/s3mail_agent/graph.py), router compaction.

Compacting the latest `messages` channel fixes its growth and stops repeatedly copying every old tool excerpt. It does not remove old checkpoints/events. The runtime still constructs `AgentCoreMemorySaver` with its defaults. In the locked 1.2.5 dependency, that means `checkpoint_format="legacy"` and `limit=None`; `get_tuple()` reads the session event history through `read_events()` before reconstructing the latest state.

**Evidence:** production saver configuration is unchanged, and the installed locked dependency's constructor, `get_tuple()` and event pagination still follow this path. The new 25-question test uses `InMemorySaver` and asserts only the latest message count. It does not exercise AgentCore restoration or bound the number of events restored.

**Required change:** validate a bounded restoration strategy, with compatible migration/recovery behavior, and test its read work as a conversation accumulates checkpoints. Do not arbitrarily cap legacy events and risk incomplete recovery. This is a source-confirmed remaining performance issue; no cloud latency/cost measurement was performed.

### C6 — P2: Trigger-to-owner command isolation remains open

**Related finding:** R11. The commit explicitly defers this issue. The trigger's runtime invocation authority, constant owner user ID and shared mode dispatcher are unchanged. It can still issue owner commands through the runtime if its credentials or execution are misused; OAuth scope limits remain an independent constraint.

**Required change:** enforce the automated caller's allowed modes at an authenticated boundary. An owner decision about implementation does not make the current boundary isolated.

## Status of the original twelve findings

“Fixed” below means the original local reproduction is addressed by code and regression tests. It does not mean deployed validation or a release gate has passed.

| Finding | Assessment of e69137d |
| --- | --- |
| R1 — trace exception content | Fixed for the reproduced path. Explicit exception/status suppression and a content scan are present; download names are also fixed. |
| R2 — rejection resumes approved action | Fixed for the reproduced sequential crash/rejection cases. The replacement decision is checkpointed before recovery. |
| R3 — repeated mailbox updates | Original crash replay fixed by the marker; stale-update protection remains racy (C3). |
| R4 — digest overflow | Change-stream pagination fixed; query fallback still loses tied overflow (C2). |
| R5 — reminder limits | Thread batching and header caps fixed; partial-thread reply detection regresses correctness and GSI reads remain unbounded (C4). |
| R6 — model outage blocks digest | Fixed: the optional overview failure now degrades to deterministic delivery. |
| R7 — digest delivery/cursor recovery | Cursor compare-and-set and basic deduplication are improvements; retry/preview reuse can skip new mail (C1). |
| R8 — checkpoint growth | Latest message state is bounded; restoration of legacy event history remains unbounded (C5). |
| R9 — unkillable extraction thread | Original timeout problem addressed with a killable/reaped child. The tests pass locally; Linux memory-limit behavior still needs the actual runtime image. |
| R10 — XLSX shared-string indices | Original reconstruction case fixed, with stronger row-value tests. |
| R11 — automated caller authority | Explicitly open and unchanged (C6). |
| R12 — unexposed injection failures pass | Original false-pass case fixed by exposure and run-error gate conditions. |

## Validation and limits

- Reviewed the exact commit diff and the changed implementation/tests/spec, plus the unchanged authorization and checkpointer paths relevant to remaining findings.
- Passed all **204 Python tests**, Ruff lint, Ruff formatting and strict mypy on an isolated commit export, using the existing locked environment; the commit does not change its dependency lockfile.
- Passed Go race tests for `internal/agentcli` and `internal/agenttrigger`; `cmd/agent` and `cmd/agent-trigger` compile and have no tests.
- Reproduced C1 by both cursor-write failure/retry and preview/normal delivery, C2 with three successive digests, C3 with a write-boundary owner change, and C4 with a long thread. These temporary probes use only synthetic mail and the existing fake server/model.
- C5 and C6 are source-confirmed; no deployed permission impersonation, AgentCore storage benchmark, paid model evaluation or personal-mail access was performed.
- Terraform was not changed in this commit, so its earlier mocked test result was not rerun. The full repository `make check` and runtime image/deployment checks were not run.

The existing tests pass but do not cover the counterexamples above. No implementation fixes, commit, merge or deployment were made during this follow-up review.

**Diagram impact:** none. This review records remaining behavior and proposed fixes without changing components, integrations, data flows, authorization boundaries, hosting or deployment stages.

## Resolution

Added 2026-10-19 after this review; the findings above are unchanged. C1–C4 were first re-run against `e69137d` with temporary probes and reproduced as described. The fixes are offline-tested only; nothing was deployed.

| ID | Resolution | Tests |
| --- | --- | --- |
| C1 | Before creating its email, a delivering run records the cursor its window ends at (`DIGEST#<window>`, `sk` `end`). A run that finds an existing email for its window returns it and advances only to that recorded end. Previews carry no keyword and record nothing. | Mail arriving before a retry appears in the next digest; a preview followed by real delivery creates a separate, complete digest |
| C2 | A capped query fallback stores the last listed email's id, and the next digest resumes after it with `anchor`/`anchorOffset`; `anchorNotFound` resumes from its time. The fake now honors the sort direction with s3mail's id tiebreak, plus anchors and `position`. | 650 emails, tied and spread, list exactly once over successive digests, ending with a full state |
| C3 | Update batches are sent with `ifInState` from the executor's check, then chained through each batch's `newState`; on `stateMismatch` the batch's emails are checked again, and the write is retried only if they are unchanged (at most three attempts). The fake advances its Email state on updates and enforces `ifInState`. | An owner change between check and write leaves the email untouched (`mail_changed`); unrelated new mail at that moment is retried to `done` |
| C4 | Each row reads its email and all later thread emails; a thread with more than 50 later emails, or a row beyond the read budget, is not reminded. The store query returns only the newest 200 candidates, newest first, between the reminder bounds. | The sent reply outside the old 20-email tail ends the reminder; a thread over the limit is not reminded; the stubbed index query requests the limit and never reads the older page |
| C5 | The runtime's saver uses `checkpoint_format="snapshot"`: restores read the latest complete checkpoint instead of replaying the event history, and legacy threads are still read by replaying them. The runtime role already allows every Memory API it uses. Rolling back to an earlier runtime abandons snapshot threads (documented in the spec). | The deployed saver is configured for snapshots with no read limit. The library's restore path was confirmed in its source; restore work against AgentCore was not measured |
| C6 | Accepted risk, not fixed (owner decision, 2026-10-19), as recorded for R11: documented in the [agent spec](../specs/agent.md#identity-and-authorization), to be revisited before `mail.submit` is granted. | — |

### Follow-up findings after `ae72106`

Added 2026-10-19. The [review of `ae72106`](agent-fix-review-ae72106.md) raised three more P2 issues (A1–A3, here C7–C9), each confirmed in source before it was fixed; the C8 fix's regression found by the [review of `48a5b2e`](agent-fix-review-48a5b2e.md#resolution) is resolved there. The C3 row above held only against the fake server, because s3mail itself did not enforce `ifInState` past the method's first state read (C9). These fixes are offline-tested only; nothing was deployed, and the agent's C3 protection holds live only once the backend change is deployed.

| ID | Finding | Resolution | Tests |
| --- | --- | --- | --- |
| C7 | The cursor's conditional write compared `at` and `state` but not `after_id`, so a stale run could overwrite newer fallback progress with the same time. | The condition compares every field, requiring the attribute to be absent when the expected cursor has none. | The stubbed write's condition names `after_id` for a resuming cursor and its absence otherwise |
| C8 | A continuing query fallback resumed only after its anchor. Mail committed later that sorts before the anchor (ingested after its SES receipt time, or imported with an earlier `receivedAt`) was never listed, and the completed query's state then covered it. | A capped page stores the Email state read before it. The next digest lists the emails created since that state that sort at or before the anchor, then continues the query; the race between that state read and the query can list an email twice, never skip one. A cursor without a state, or expired changes (logged), resumes without the check. | Mail landing between two digests earlier than, or in the same second as, the anchor is listed exactly once; it was never listed before the fix |
| C9 | `Email/set` and the other methods taking `ifInState` checked it once. Each object's commit loop then read a fresh state, so a change landing before the commit, or one that made the commit lose the sequence race and retry, was overwritten. | Every commit of the call checks that the guarded type is unchanged since the client's state apart from the call's own commits, also on each retry (`Email/set`, `Email/import`, `Mailbox/set`, `Identity/set`, `EmailSubmission/set`; success hooks run unguarded as before). A change before the first commit is the method-level `stateMismatch`; after a partial apply, each remaining object is refused with a per-object `stateMismatch`, and creates after the first refusal archive nothing. The executor retries only the refused emails after checking them again; the fake server now commits per email, as s3mail does. | Go: a change before the first commit, during a commit retry and between two objects is refused (all three failed on the old code), a change to another type still retries, and `Mailbox/set` shares the guard. Agent: new mail after part of a batch retries only the rest; an owner change there stops with `mail_changed` |
