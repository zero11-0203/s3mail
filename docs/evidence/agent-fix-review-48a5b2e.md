# Review of agent fixes in 48a5b2e

[Documentation index](../README.md) · [Previous review](agent-fix-review-ae72106.md) · [Original findings](agent-implementation-review-2026-10-19.md)

**Reviewed commit:** `48a5b2eb96e453fe9df142459e2ebf1d32a19fa2`, “Hold ifInState through every commit; close C7-C9”, dated 2026-10-19. This is the latest commit on `worktree-agent-review-fixes`; `main` still points to `adfa5ed`. Review and execution used an isolated export of the exact revision. Source line numbers below refer to that revision.

**Assessment:** the three findings from the previous review are addressed, but this change introduces one confirmed **P2 digest-coverage regression**. It therefore does not resolve all correctness issues. The shared-runtime authority issue R11/C6 is now an owner-accepted risk, as recorded in parent commit `544247f`; it is accepted rather than technically fixed.

## F1 — A deleted page tail makes the digest abandon its remaining query backlog

**Location:** [digest.py](../../agent/src/s3mail_agent/digest.py), `_changes`, lines 176–180; the resulting cursor selects the change-stream branch at lines 113–123. **Priority: P2.**

When a fallback query is capped, the code fetches its last email's `receivedAt` to establish the next anchor. If that email was destroyed after `Email/query` returned, the missing-header branch now returns the newly captured Email state with the previous anchor. On the first fallback page, that previous anchor is absent. The stored cursor consequently has a valid `state` and no `after_id`, even though the query backlog is unfinished.

The next digest interprets that combination as a completed query and uses only `Email/changes`. The captured state already covers the creation of every email in the backlog, including those beyond the first page. Those emails are therefore absent from subsequent changes and never listed. The deletion itself need not be lost or misreported for this failure to occur: a change page containing only the destruction still has no new creations to list.

**Reproduced with the actual agent implementation and its synthetic JMAP transport:**

1. Give the digest an expired state and a time cursor before 650 live messages, `M000` through `M649`, all with the same `receivedAt`.
2. Let `Email/query` return its first 300 IDs and total 650.
3. Immediately after that query, destroy `M299` and advance the Email state, before the agent's separate header read.
4. Run three successive digests without further arrivals.

Observed results:

| Run | Listed emails | Stored cursor |
| --- | --- | --- |
| First | 299 | `state=s650`, original cursor time, `after_id=None` |
| Second | 0 | `state=s651`, current digest time, `after_id=None` |
| Third | 0 | `state=s651`, current digest time, `after_id=None` |

Only one `Email/query` was issued across all three runs. The 350 live messages `M300`–`M649` were never listed. Original mail remains intact and accessible; the defect loses digest coverage. This is distinct from the documented fallback limitation for mail received before the cursor time: every omitted message in this reproduction matches the fallback's time filter and was present before the first query.

The previous implementation returned `state=None` from this missing-tail branch, keeping the next run in query mode. Retaining a watermark is necessary for the new late-arrival protection, but the new representation cannot distinguish this unfinished query without an anchor from an ordinary change-stream cursor.

**Required change:** preserve unfinished-query status when the page tail disappears. A surviving page member can provide an anchor, an explicit continuation mode can retain the watermark without an anchor, or the run can fail without advancing its cursor and retry from the original position. Do not persist an ordinary state-only cursor until the query backlog is covered. Add a regression test that destroys the tail between `Email/query` and the anchor-header read; cover both an expired cursor and the first digest with no prior cursor.

## Status of the previous findings

| Previous finding | Assessment of 48a5b2e |
| --- | --- |
| A1 / C7 — cursor conditional write omits the anchor | **Resolved offline.** The emitted DynamoDB condition includes equality or absence for all three cursor fields. Repeating the stale-anchor probe rejects the write and preserves `M599` instead of replacing it with `M499`. This is request-level validation, not a live DynamoDB test. |
| A2 / C8 — new mail behind a continuing query's anchor is skipped | **Original reproduction resolved.** The cursor retains a pre-query state, and `_behind` reconciles creations before the anchor. The 301-message reproduction now lists `300, 2, 0`, including the intervening arrival exactly once. Added tests also cover an earlier timestamp and a timestamp tie. F1 is a separate deletion race in the new fallback handling. |
| A3 / C9 — backend retries past `ifInState` | **Resolved in the tested conflict paths.** The shared Go guard checks the type state on every commit attempt, including retries; it reports a method error before any commit and per-object refusal after partial application. Go tests inject a competing move before the first commit, during its read/commit race and between objects. The Python executor retries only refused objects after checking them again. |
| R11 / C6 — automated invocation also permits owner commands | **Accepted risk, unchanged technically.** `544247f` records the owner's acceptance for `mail.read` and `mail.write`. The owning spec and scope-grant procedure require revisiting it before granting `mail.submit`. It is not counted here as an unresolved, unaccepted finding. |

The review also covered the changed paths for security, resource bounds, error handling and design. No additional actionable security or performance issue was confirmed in this diff. Catch-up work retains its page and email caps, header reads remain batched, and the shared state guard adds constant per-call state. These are source-level observations, not load-test or deployed-security evidence. The cursor's ambiguous representation is the design problem behind F1.

## Validation

| Check | Result |
| --- | --- |
| Python suite on the commit export | **220 passed**, one upstream Pydantic deprecation warning |
| Ruff lint and format checks | Passed; 43 files formatted |
| Strict mypy on `src` and `evals` | Passed; 26 source files |
| Full Go suite, `go test -race ./...` | Passed, including the new shared state-guard tests |
| Previous cursor-condition and late-arrival probes | Both original failures resolved, as described above |
| Synthetic page-tail deletion probe | Confirmed F1: `299, 0, 0` listed and 350 live messages omitted |
| Commit whitespace and review-document local links | Checked |

Python checks used the existing locked environment with the export's source and tests; this commit does not change dependencies. Temporary probes were written only in the isolated export. The checkout changes for this review are this report and its documentation-index entry; earlier review files already present were preserved. No implementation change, commit, merge, deployment, personal-mail access or paid model evaluation was performed.

The complete `make check`, Terraform checks, image checks, deployed DynamoDB/AgentCore/Identity behavior and end-to-end Python-to-Go network integration were not exercised. Passing these offline checks does not establish a release gate.

**Diagram impact:** none. This report adds evidence without changing AWS components, integrations, data flows, authorization boundaries, hosting or deployment stages.

## Resolution

Added 2026-10-19 after this review; the finding above is unchanged. F1 was first reproduced against `48a5b2e` with a temporary probe, from an expired cursor and from no cursor: `295, 0, 0` emails listed, with a state-only cursor and one `Email/query`. The fix is offline-tested only; nothing was deployed.

| ID | Resolution | Tests |
| --- | --- | --- |
| F1 | When a capped page's last email is gone by the time the digest reads it, the digest reads the page again and resumes after the last listed email that still exists; any after it were destroyed, so nothing is listed twice. If the whole page is gone, it keeps the earlier anchor with the new state, or, on a first page with no anchor, stores no state, so the next digest queries from the same time again instead of reading the cursor as a finished query. A state is never stored without an anchor while the query is unfinished. The whole-page handling was later replaced by an immediate re-query, after the [review of `e14c353`](agent-fix-review-e14c353.md#resolution) (G1). | The tail, or the whole first page, destroyed between `Email/query` and the header read, from an expired cursor and from no cursor: every surviving email is listed exactly once over four digests, ending with a full state. All four cases failed on `48a5b2e` |
