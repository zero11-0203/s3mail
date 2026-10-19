# Review of agent fixes in e14c353

[Documentation index](../README.md) · [Previous review](agent-fix-review-48a5b2e.md) · [Original findings](agent-implementation-review-2026-10-19.md)

**Reviewed commit:** `e14c353ffc191dc995f5ce7b9730dfd9ab055b46`, “Keep a fallback query going when its page end is destroyed (F1)”, dated 2026-10-19. This is the latest commit on `worktree-agent-review-fixes`; `main` remains at `adfa5ed`. Review and tests used an isolated export of this exact revision. Line numbers refer to the reviewed revision.

**Assessment:** the original F1 deleted-tail reproduction is fixed. Independent probes also confirm recovery after one entire page disappears, including a continuing page with an existing anchor. However, the new no-state fallback can leave a delivered digest's end cursor identical to its start. That interacts with delivery deduplication and produces one confirmed **P2** finding below. This commit therefore does not close every digest correctness issue.

## G1 — Repeated whole-page deletions can stall subsequent mail digests

**Location:** [digest.py](../../agent/src/s3mail_agent/digest.py), `_changes`, lines 183–188. **Interaction:** `window_key`, lines 380–383, and `run_digest`'s delivery recovery, lines 433–447 and 492–502. **Priority: P2.**

When an entire capped page disappears and there is no earlier anchor, the new branch returns `Window(ids, None, None, after)`. This correctly keeps the next run in query mode. But if the run already started from `Cursor(None, after, None)`, the resulting cursor equals its starting cursor.

For `deliver="mail"`, the digest is still created and its end cursor recorded under the hash of that starting cursor. The next run has the same hash, finds the previous digest and its saved end, writes back the same cursor, and returns `reused` before reading changes or the fallback backlog. Lease expiry does not resolve this cycle. It continues while the saved digest and end record remain available, even if nothing else is deleted and new mail arrives.

**Reproduced through the actual agent's mail-delivery path with synthetic mail:**

1. Start with an expired cursor and 650 messages, `M000`–`M649`, all matching the fallback time filter.
2. On the first run, destroy its whole 300-message page between `Email/query` and the header read. The run delivers an empty digest and stores `Cursor(None, original_time, None)`.
3. On the second run, destroy the next 300-message page at the same boundary. Its end cursor is identical to its start, and another empty digest is delivered.
4. Stop deleting messages, deliver one new message, and run the mail digest twice more. All runs are 11 minutes apart, exceeding the 10-minute lease.

| Run | Result | Cursor after the run |
| --- | --- | --- |
| First | Empty digest created | `Cursor(None, original_time, None)` |
| Second | Another empty digest created | Same cursor |
| Third | Second digest returned with `reused: true` | Same cursor |
| Fourth | Second digest returned with `reused: true` | Same cursor |

Across the four runs, only two fallback queries execute. The remaining **50 original messages and the new arrival** are never listed. Original mail remains accessible; the failure stalls digest coverage and reports success. A single deletion event from an already stored no-state, no-anchor cursor is sufficient; the first event above shows how this commit naturally produces that starting state.

The four added regression cases use the default return delivery and destroy a page only once. Return delivery has no saved-digest reuse shortcut, so those cases cannot detect this interaction. Separate probes of the same one-event cases with mail delivery also pass; the finding specifically concerns an unfinished window whose end equals its start.

**Required change:** prevent successful delivery of an unfinished window from creating an unchanged continuation identity. For example, abort/retry a no-progress window before recording or delivering it, or persist an independent continuation generation that distinguishes the next unit of work from recovery of the previous delivery. Preserve safe recovery after an unknown email-create result. Add a `deliver="mail"` regression with two successive whole-page deletions, lease expiry and a subsequent arrival; later runs must reach the surviving mail instead of repeatedly returning the old digest.

## Other review results

- **Previous F1:** resolved for the original tail deletion. The independent 650-message reproduction now lists `299, 300, 50`, with every surviving message listed once. One whole-page deletion lists `0, 300, 50` and also completes.
- **Resource bounds:** the missing-tail path adds a reread of at most 300 headers, batched within the server's advertised get limit. No unbounded loop or additional actionable performance issue was confirmed. No load benchmark was run.
- **Security and design:** no new permissions, external data sources, model instructions or telemetry content are introduced. No additional actionable security issue was confirmed in this diff. G1 exposes the design assumption that a successfully delivered window always changes its cursor.
- **R11/C6:** remains an owner-accepted shared-runtime authority risk for `mail.read` and `mail.write`, with reconsideration required before granting `mail.submit`. This commit does not change that status.

## Validation

| Check | Result |
| --- | --- |
| Python suite on the exact export | **224 passed**, one upstream Pydantic deprecation warning |
| Ruff lint and format checks | Passed; 43 files formatted |
| Strict mypy on `src` and `evals` | Passed; 26 source files |
| Independent deletion probes | 16 cases passed: expired or absent cursor, first or continuing page, one tail or whole page destroyed, return or mail delivery |
| Repeated whole-page deletion with mail delivery | G1 reproduced: subsequent runs reuse the same digest; 51 live messages remain unlisted |
| Commit whitespace and documentation links/headings | Checked |

Checks used the existing locked Python environment with the export's source and tests; dependencies are unchanged. Extra probes and synthetic fixture adjustments were confined to the temporary export; the agent implementation was not edited. This review adds only this Markdown report and its index entry, preserving earlier reports already present in the checkout.

The full `make check`, Go suite, Terraform checks, runtime image checks, deployed AWS integrations and paid model evaluations were not run for this commit. Go and infrastructure are unchanged from the preceding review. Offline checks do not establish a release gate, and no deployment or personal-mail access was performed.

**Diagram impact:** none. This report changes no AWS component, integration, data flow, authorization boundary, hosting or deployment stage.

## Resolution

Added 2026-10-19 after this review; the finding above is unchanged. G1 was first reproduced against `e14c353` with a temporary probe through mail delivery: after two whole-page deletions, both later runs returned the earlier digest (`reused`) with the cursor unchanged, and no email was listed. The fix is offline-tested only; nothing was deployed.

| ID | Resolution | Tests |
| --- | --- | --- |
| G1 | A capped fallback page that is entirely gone before the digest reads it is queried again from the same place within the run, up to three times, so the run resumes at the mail that survived instead of storing an unfinished cursor. If every attempt's page is gone, the run stops with `digest_retry` before any model call, recorded end or email, leaving the cursor where it was. As a backstop for the design assumption this review identified, a mailed run whose window would end exactly where it started also stops with `digest_retry`, since that end would name the same window and later runs would reuse its digest. Recovery after an unknown email-create result is unchanged: no digest email is created on these paths. | Mailed digests with the whole page of each of two successive runs destroyed, lease expiry and a later arrival list every surviving email and the new one once, never returning `reused` (fails on `e14c353` with `reused: true`); three destroyed pages in one run give `digest_retry` with no email and an unchanged cursor, and the next run lists the survivors; a window ending at its start is not delivered |
