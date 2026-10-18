# Review of agent fixes in ae72106

[Documentation index](../README.md) · [Previous review](agent-fix-review-e69137d.md) · [Original findings](agent-implementation-review-2026-10-19.md)

**Reviewed commit:** `ae7210696fbf0330cfb9f052b4d6cd6f3b943c3b`, “Close the gaps the e69137d fix review found (C1-C5)”, dated 2026-10-19. The review and tests use an isolated export of that exact commit. Source locations below refer to this revision.

**Assessment:** the earlier fixed-mailbox examples are addressed, but three correctness gaps remain. Two concern the new fallback cursor, and a pre-existing backend precondition defect prevents the claimed C3 concurrency fix from holding end to end. R11/C6 remains explicitly deferred. The commit is useful progress, but its claim to close C1–C5 needs these qualifications.

All three findings below are **P2**: concrete concurrency or digest-coverage problems. None deletes original mail, and no production incident was demonstrated.

## A1 — Compare-and-set omits the new cursor anchor

**Location:** [store.py](../../agent/src/s3mail_agent/store.py), `DynamoTable.set_cursor`, lines 258–273. **Related:** C2 and the cursor concurrency protection from R7.

`Cursor` now includes `after_id`, and the query fallback relies on that field to distinguish pages sharing a timestamp. The DynamoDB conditional write still compares only `at` and `state`. Two different fallback pages can therefore look identical to its condition: both have `state` absent and the same `at`, while their anchors differ.

For example, a stale run read `(None, T, M299)` and wants to store `(None, T, M499)`. Another run has already advanced the cursor to `(None, T, M599)`. The generated condition is:

```text
#a = :at AND attribute_not_exists(#s)
```

That condition is true for the newer row, so the stale write can replace anchor `M599` with `M499`. DynamoDB evaluates the supplied condition against the existing item; it does not implicitly compare fields omitted from the expression. See [DynamoDB conditional writes](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/Expressions.ConditionExpressions.html).

**Evidence:** an offline request-level probe called the real `DynamoTable.set_cursor()` with an injected client that evaluated its emitted condition. The stale update was accepted and the stored anchor regressed from `M599` to `M499`. The same operation against `MemoryTable` was rejected, because it compares the complete dataclass. This is not a live DynamoDB test; the incorrect predicate is confirmed directly in the generated request. The new store test checks anchor serialization, not anchor-aware conditional equality.

**Impact:** overlapping manual/scheduled digest work can regress fallback progress and repeat summaries. In-memory concurrency tests give stronger guarantees than the deployed adapter.

**Required change:** condition on the entire expected cursor, including `after_id` equality or absence, or use an independent version field. Test stale writes between different anchors at the same timestamp against the deployed adapter's generated condition and the in-memory store.

## A2 — A new arrival behind the anchor is permanently skipped

**Location:** [digest.py](../../agent/src/s3mail_agent/digest.py), `_changes`, lines 114–142. **Related:** C2.

The anchor solves pagination over an unchanged backlog, including timestamp ties. However, each resumed page queries the current mailbox and obtains a new Email state. A message created between pages can sort before the saved anchor—for example, a delayed receipt or import with the same `receivedAt` and a smaller ID. The anchored query excludes it, but the final stored Email state includes its creation. Later `Email/changes` calls therefore do not recover it.

**Reproduced using the actual agent implementation and its updated sorted/anchored fake:**

1. Start with an expired change state and 301 messages, `M000` through `M300`, sharing a timestamp.
2. The first digest lists 300 and saves anchor `M299`.
3. Deliver `A000` with that same timestamp before the next digest. It sorts before the anchor.
4. The second digest lists only `M300` and saves complete state `s302`.
5. The third digest lists zero. `A000` never appears in any digest.

Observed counts were `300, 1, 0`. Unlike the fixed 650-message tests, this reproduction changes the mailbox between fallback pages. The underlying email remains accessible in the inbox; its digest coverage is lost.

**Required change:** preserve a change-stream watermark from the start of fallback traversal and reconcile creations arriving during that traversal before advancing past them, or use an equivalent stable snapshot/continuation design. A mutable query anchor by itself does not prove coverage of the newly captured state. Add inter-page arrivals before the anchor, both within a timestamp tie and with an older receipt time.

## A3 — The real backend does not preserve the new ifInState protection through a conflict

**Changed integration:** [proposals.py](../../agent/src/s3mail_agent/proposals.py), lines 300–324. **Underlying implementation:** [email_set.go](../../go/internal/jmap/email_set.go), lines 24–29 and `updateEmail`; [mailbox.go](../../go/internal/jmap/mailbox.go), `commitLoop`, lines 625–637. **Related:** C3.

The agent correctly adds `ifInState` to its writes and handles `stateMismatch`. The real Go `Email/set` implementation checks that argument once on entry, then executes mutations through `commitLoop`. If an owner transaction wins before the first agent commit, the loop reloads the newer state and reapplies the patch without rechecking the supplied precondition. It can return success instead of `stateMismatch`.

This backend defect predates `ae72106`; it is not introduced by the new Python code. It nevertheless leaves the claimed end-to-end fix incomplete. The [storage consistency contract](../specs/storage-sync.md#dynamodb-storage-and-consistency-contract) already requires guarding the first commit against `ifInState`, consistent with the standard [JMAP set precondition](https://www.rfc-editor.org/rfc/rfc8620.html#section-5.3).

**Reproduced with the real Go handler and its transaction-planning test store:**

1. Start at Email state `1-3` with the target in Inbox.
2. Send the agent-style move patch, removing Inbox and adding Archive, with `ifInState: "1-3"`.
3. At the first commit boundary, commit a valid owner move to Trash using the same store sequence guard.
4. The agent's first commit conflicts. The backend retries it against the newer state and succeeds.

Observed response and final membership:

```text
Email/set success: oldState=1-3, newState=1-5, target in updated
target.mailboxIds = {archive: true, trash: true}
```

The new Python fake's atomic-looking method check cannot exercise this storage conflict. The temporary Go probe passed by asserting the faulty behavior; it does not mean the precondition held.

**Required change:** enforce the shared server's first-commit precondition across transaction conflicts before any object has been applied, and return `stateMismatch` rather than retrying that first mutation against a different state. Preserve proper per-object reporting after partial set success. Add the conflict test to the Go suite and use it to validate the agent integration. This should be a shared JMAP fix, with no agent-specific server shortcut.

## Status of the previous review

| Previous finding | Assessment of ae72106 |
| --- | --- |
| C1 — retry reuses an email but advances too far | The original reproduction is fixed: the end cursor is recorded before creation and reused on recovery; previews have no delivery keyword. New incoming mail is covered by a later digest in the added tests. |
| C2 — tied timestamp overflow | The unchanged-backlog reproduction is fixed, including multiple tied pages. Anchor-aware conditional updates and concurrent arrivals still need A1/A2. |
| C3 — update validation/write race | The agent now uses the proper protocol argument, but the real backend conflict path still defeats it (A3). |
| C4 — sent reply outside sampled tail | The original case is fixed. All later members are checked within the budget, oversized threads are skipped, and the store query limits candidates. |
| C5 — unbounded legacy restore | New writes use the dependency's snapshot format, whose source has a bounded latest-snapshot restore path. Legacy migration, actual AWS read work and rollback behavior were not exercised here. The spec explicitly documents the rollback incompatibility. |
| C6 / R11 — automated caller can issue owner commands | Explicitly open and unchanged. This review does not duplicate that finding or treat it as a new regression. |

## Validation

| Check | Result |
| --- | --- |
| Python suite on the commit export | **215 passed**, one upstream Pydantic deprecation warning |
| Ruff lint and format checks | Passed; 43 files formatted |
| Strict mypy on `src` and `evals` | Passed; 26 source files |
| A1 cursor-condition probe | Confirmed the generated predicate accepts a stale different-anchor cursor; the in-memory store rejects it |
| A2 inter-page arrival probe | Reproduced permanent digest omission with counts `300, 1, 0` |
| `go test -race ./internal/jmap -run '^TestReviewAe72106' -v` | Temporary synthetic probe reproduced A3 through the actual handler and sequence-guarded test store |
| Commit whitespace and review-document links | Checked |

Tests used the existing locked Python environment; this commit does not change dependencies. The temporary probes live only in the isolated review export, not the repository's regression suites. No implementation was edited in the user's checkout, and no commit, merge, deployment, personal-mail access or paid model evaluation was performed. The complete `make check`, Terraform checks, runtime image checks and deployed AgentCore/Identity validation were not run for this review.

**Diagram impact:** none. This document records findings and proposed remedies without changing AWS components, integrations, flows, authorization boundaries, hosting or deployment stages.

## Resolution

Added 2026-10-19 after this review; the findings above are unchanged. All three were confirmed in source and fixed in `48a5b2e` as C7 (A1), C8 (A2) and C9 (A3); the [resolution table](agent-fix-review-e69137d.md#follow-up-findings-after-ae72106) records each fix and its tests. The [review of `48a5b2e`](agent-fix-review-48a5b2e.md) found the three addressed and one regression in the C8 fix (F1), resolved there. The fixes are offline-tested only; nothing was deployed.
