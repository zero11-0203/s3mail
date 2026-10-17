# JMAP RFC conformance review

[Documentation index](../README.md) · [JMAP](../specs/jmap.md) · [Conformance matrix](../specs/jmap-conformance.md)

**Status:** code review done on 2026-10-17 against commit `d62bc03`. It compares `go/internal/jmap` and `go/internal/mailparse` with [RFC 8620](https://www.rfc-editor.org/rfc/rfc8620) (JMAP Core) and [RFC 8621](https://www.rfc-editor.org/rfc/rfc8621) (JMAP Mail). This page records findings, not requirements. Requirements live in [JMAP](../specs/jmap.md), and the advertised surface is in the [conformance matrix](../specs/jmap-conformance.md). Most findings come from reading the code. One was reproduced with a scratch test, as noted. `go test ./internal/jmap/... ./internal/mailparse/...` passes at this commit.

## Summary

The implementation follows both RFCs closely. Every method in the conformance matrix exists and is registered under the right capability. The request envelope, result references, `/changes`, `/query`, `/queryChanges`, `/set` and the EventSource stream match the RFC semantics, with a few deviations. The review found:

| Severity | Count | Items |
| --- | --- | --- |
| High (crash) | 1 | [F1](#f1-nested-multipartalternative-panics-the-body-list-algorithm) |
| MUST-level gaps | 3 | [F2](#f2-no-emaildelivery-push-type), [F3](#f3-query-limit-not-reported-when-the-server-caps-results), [F4](#f4-from-to-and-subject-sorts-use-the-wrong-keys) |
| Lenient input validation | 1 group | [F5](#f5-emailset-create-accepts-input-rfc-8621-46-forbids) |
| Minor deviations | 1 group | [F6](#f6-minor-deviations) |

## Findings

### F1. Nested `multipart/alternative` panics the body-list algorithm

- **Where:** `go/internal/mailparse/bodies.go:54` and `:71`, called from `go/internal/jmap/email_get.go:263` and `go/internal/search/extract.go:43`.
- **RFC:** RFC 8621 §4.1.4, the `parseStructure` pseudo-code.
- **Problem:** The Go port is faithful to the pseudo-code. When `inAlternative` is set, the pseudo-code sets `htmlBody` or `textBody` to null, then passes it into a nested `multipart/alternative`. There the alternative branch appends to it unconditionally. In JavaScript that is a TypeError. In Go it is a nil pointer dereference.
- **Reproduced** with a scratch copy of the package. This valid message panics `BodyLists()`:

  ```text
  multipart/alternative
  └─ multipart/mixed
     ├─ text/plain
     └─ multipart/alternative
        ├─ text/plain
        └─ text/html
  ```

- **Impact:** Anyone can send such a message.
  - `Email/get` builds each Email in a goroutine (`email_get.go:201`), and the code has no `recover()` anywhere. Fetching any body property of that Email crashes the API Lambda instance, every time.
  - `Email/parse` goes through the same `buildEmail` call.
  - The search indexer calls the same function, so the message can also block indexing.
- **Fix direction:** Guard the alternative branch against nil lists, for example by skipping a list that is no longer being filled. Add the message above as a `mailparse` test and a fuzz seed. Also fuzz `BodyLists`; today `FuzzParse` covers only `Parse`.

### F2. No `EmailDelivery` push type

- **Where:** `go/internal/jmap/push.go:25`.
- **RFC:** RFC 8621 §1.5: servers with `urn:ietf:params:jmap:mail` "MUST support pushing state changes for a type called `EmailDelivery`". Its state must change when a new Email is added, and should not change on other Email changes.
- **Problem:** `pushTypes` lists only `Email`, `Mailbox`, `Thread`, `Identity` and `EmailSubmission`. `types=EmailDelivery` is silently dropped, and `types=*` leaves it out.
- **Fix direction:** Track a sequence that only delivery or import commits advance (ingest, `Email/import`, possibly `Email/set` create). Expose it in the StateChange.

### F3. Query `limit` not reported when the server caps results

- **Where:** `go/internal/jmap/mailbox.go:520-548` (`page`).
- **RFC:** RFC 8620 §5.5, response `limit`: "This is only returned if the server set a limit or used a different limit than that given in the request."
- **Problem:** With no `limit` in the request, results are silently capped at 1024 and `limit` is omitted. `limit` is only returned when the client asked for more than 1024. A client that omits `limit` and `calculateTotal` cannot tell the list was truncated.
- **Fix direction:** Set `limitOut` whenever the cap applies, including when the request had no `limit`.

### F4. `from`, `to` and `subject` sorts use the wrong keys

- **Where:** `go/internal/jmap/email_query.go:191` (`firstAddress`) and `:225`.
- **RFC:** RFC 8621 §4.4.2:
  - `from` and `to` sort by the first address's **name**, or by its email if the name is null or empty.
  - `subject` sorts by the **base subject** (RFC 5256 §2.1), which drops `Re:`, `Fwd:` and similar prefixes and trailers.
- **Problem:**
  - `from` and `to` always compare the lowercased email address.
  - `subject` compares the whole lowercased subject.
  - Clients that sort by sender or subject get a different order than the RFC defines. For example, `Re: Foo` sorts under R, not F.
  - `i;unicode-casemap` is approximated with `strings.ToLower`. That is close but not the RFC 5051 algorithm.
- **Fix direction:** Store or derive the display name and base subject in Email metadata. Compare them with a casemap collation.

### F5. `Email/set` create accepts input RFC 8621 §4.6 forbids

`go/internal/jmap/compose.go` enforces most §4.6 rules. These are correctly rejected:

- `Content-*` on the Email
- `Content-Transfer-Encoding`
- `charset` with `partId`
- truncated or encoding-problem `bodyValues`
- `bodyStructure` together with the shortcut properties
- duplicate header properties

These MUST NOT rules are not enforced:

- **`headers` property:** "MUST NOT be given on either the top-level Email or an EmailBodyPart". It is accepted on both (`compose.go:62`, `customHeaders`).
- **Forbidden parsed forms:** "Header fields MUST NOT be specified in parsed forms that are forbidden for that particular field". For example, `header:Subject:asAddresses` is accepted. `Email/get` (`email_get.go:43`, `parseHeaderProp`) likewise returns any form for any header, where §4.1.2 says such a request is an `invalidArguments` error.
- **Repeated headers:** A `bodyStructure` part "MUST NOT contain a property representing a header field that is already defined on the top-level Email". There is no cross-check between part headers and Email headers.
- **`size` with `partId`:** "The `size` property MUST be omitted if a partId is given". It is accepted and ignored.

These are lenient rather than unsafe: header injection is still blocked by `validRawValue`. Rejecting them would match the RFC and catch client bugs earlier.

### F6. Minor deviations

| Area | Code | RFC | Observation |
| --- | --- | --- | --- |
| `/get` with `ids: null` | `mailbox.go:165` | 8620 §5.1 | `Mailbox/get` returns every Mailbox even beyond `maxObjectsInGet` (64). The RFC allows "all" only within that limit. Harmless at personal scale. `Email/get` and `Thread/get` handle this correctly with `requestTooLarge` |
| `/changes` `maxChanges` | `changes.go:112` | 8620 §5.2 | When one commit holds more changes than `maxChanges`, the server returns `cannotCalculateChanges`, which forces a full resync. That is allowed but costly. An `Email/set` or `onDestroyRemoveEmails` commit stays small, so this is rare |
| SetError types | `mailbox.go:606`, `submission.go:433` | 8620 §5.3 | `serverFail` (busy or commit failure) and `invalidArguments` (creation not an object) are used as SetError types. Neither is defined for SetError. `invalidProperties` fits the second; a method-level `serverFail` or `rateLimit` fits the first |
| Mailbox create response | `mailbox.go:829` | 8620 §5.3 | `created` should include properties the server defaulted. `parentId` and `role` (both null by default) are left out when the client omitted them |
| `Email/parse` defaults | `email_import.go:236` | 8621 §4.9 | The default property list adds `blobId` and `size` to the RFC list. That is harmless extra data |
| EventSource ping | `push.go:158` | 8620 §7.3 | The RFC times pings from "the previous event". The code times them from the previous ping, and on a 3 s poll tick. Pings can be early, which is harmless, or up to 3 s late |
| Problem types | `http.go`, `push.go` | 8620 §3.6.1 | `forbidden`, `notFound`, `serverFail` and `invalidArguments` are sent as `urn:ietf:params:jmap:error:*` request-level problem types. Only `unknownCapability`, `notJSON`, `notRequest` and `limit` are registered at that level. Clients still get correct HTTP statuses |
| Download caching | `http.go:184` | 8620 §6.2 | The RFC recommends long cache times with `immutable`. The code sends `no-store`. This is a deliberate security choice ([security](../specs/security.md)) and not a conformance failure |

## Verified as conforming

Checked line by line against the RFC text:

- **Session (8620 §2, 8621 §1.3):**
  - All required core limits and `collationAlgorithms` are present.
  - The mail and submission capabilities are empty objects at session level and carry the required account-level properties.
  - The URL templates contain all required variables.
  - `sessionState` matches the session's `state`.
- **Request processing (8620 §3):**
  - `notJSON`, `notRequest`, `unknownCapability` and `limit` are checked before any method runs.
  - A method whose capability is missing from `using` gets `unknownMethod`.
  - `createdIds` is echoed only when the request sent it.
  - Each call's argument object is decoded strictly.
- **Result references (8620 §3.7):**
  - Lookup uses the first response with the call id, then checks the name.
  - JSON Pointer supports `*` and flattens arrays.
  - A `foo`/`#foo` conflict is `invalidArguments`.
- **`/changes` (8620 §5.2):**
  - Creates, updates and destroys are coalesced, and created-then-destroyed ids are dropped.
  - Pages end at commit boundaries with `hasMoreChanges`.
  - `maxChanges ≤ 0` is `invalidArguments`.
  - Pruned or foreign states are `cannotCalculateChanges`.
  - `Mailbox/changes` `updatedProperties` follows 8621 §2.2.
- **`/query` (8620 §5.5):**
  - Negative `position` counts from the end.
  - `anchor`/`anchorOffset` clamp at 0, and an unknown anchor gives `anchorNotFound`.
  - A negative `limit` is `invalidArguments`.
  - `canCalculateChanges` is false for index-backed queries.
- **`/queryChanges` (8620 §5.6):**
  - Every changed id is removed, and current results are re-added in index order.
  - Ignoring `upToId` is permitted.
  - Thread-dependent queries widen the change set to whole threads.
  - Exceeding `maxChanges` gives `tooManyChanges`.
- **`/set` (8620 §5.3):**
  - Creates run before updates, and updates before destroys.
  - `maxObjectsInSet` counts all three together.
  - `ifInState` gives `stateMismatch`.
  - `#creationId` references work in ids, `parentId`, `mailboxIds`, `emailId` and `blobId`.
  - `not*` maps are null when empty.
  - PatchObject prefix conflicts give `invalidPatch`.
- **Mailbox (8621 §2):**
  - Filters, the `sortOrder`/`name` sorts, `sortAsTree` and `filterAsTree` work.
  - Sibling names are unique, and cycle and depth checks are in place.
  - `mailboxHasChild` and `mailboxHasEmail` are returned.
  - `onDestroyRemoveEmails` destroys orphaned Emails.
- **Email (8621 §4):**
  - Default `properties` and `bodyProperties` match the RFC lists exactly.
  - The `header:{name}[:as{form}][:all]` grammar is parsed and the last instance wins.
  - `bodyValues` turns CRLF into LF, truncates on a character boundary and sets `isTruncated` and `isEncodingProblem`.
  - Every `Email/query` filter condition except `header` follows the RFC's inclusive/exclusive boundaries. An unknown condition gives `unsupportedFilter`, as 8620 §5.5 allows.
  - `collapseThreads` keeps the first result of each thread.
  - Keyword syntax and lowercasing are correct.
  - `mailboxIds` must hold at least one mailbox.
  - The `created` objects of `Email/set` and `Email/import` carry `id`, `blobId`, `threadId` and `size`.
  - `Email/parse` nulls the object-only properties.
- **Thread (8621 §3):** `emailIds` are oldest first.
- **EmailSubmission (8621 §7):**
  - When the envelope is omitted, it is derived from Sender/From and To/Cc/Bcc. The identity check makes this equivalent to the RFC's rule.
  - SMTP parameters are refused.
  - `forbiddenFrom`, `forbiddenMailFrom`, `invalidRecipients`, `noRecipients`, `tooManyRecipients` and `cannotUnsend` are returned where the RFC defines them.
  - `onSuccessUpdateEmail`/`onSuccessDestroyEmail` run as an implicit `Email/set` with the same call id.
  - Bcc is stripped before relay (`relay/relay.go:118`).
  - `deliveryStatus` uses the RFC values.
- **Blobs (8620 §6):**
  - Upload returns 201 with `accountId`, `blobId`, `type` and `size`, and uploads are kept 24 hours (more than the RFC minimum).
  - Download honours `type` and `name`.
  - `Blob/copy` and `Email/copy` correctly report `fromAccountNotFound` on a single-account server.
- **EventSource (8620 §7.3):**
  - `types`, `closeafter=state|no` and `ping` are supported.
  - The ping interval is clamped to 300 s and reported in `interval`, which the RFC permits.
  - The `StateChange` shape is correct.

## Out of scope or optional

- **`PushSubscription/*` (8620 §7.2):** Not implemented. §7 describes two push mechanisms but does not require both. EventSource covers aerc and the planned webmail.
- **`VacationResponse` (8621 §8):** Not implemented. It has its own capability (`urn:ietf:params:jmap:vacationresponse`), so leaving it unadvertised conforms.
- **`header` filter condition (8621 §4.4.1):** Unsupported. It returns `unsupportedFilter`, which RFC 8620 §5.5 allows.
- **Multiple accounts:** Not supported. `/copy` methods are single-account by design.
- **WebSocket (RFC 8887):** Not advertised.

## Suggested order

1. **F1:** Fix the nil-list guard and add a regression test. Any inbound mail can trigger it.
2. **F3:** A one-line fix.
3. **F2:** Add `EmailDelivery` push state. Once it exists, a client can tell real new mail from flag changes and only alert on new mail.
4. **F4:** Fix the sort semantics.
5. **F5 and F6:** Tighten as convenient.
6. **Docs:** Update the [conformance matrix](../specs/jmap-conformance.md) in the same change as each fix.
