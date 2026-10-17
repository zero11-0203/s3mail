# JMAP RFC conformance review

[Documentation index](../README.md) · [JMAP](../specs/jmap.md) · [Conformance matrix](../specs/jmap-conformance.md)

**Status:** source review on 2026-10-17 against implementation commit `d62bc03`. It compares `go/internal/jmap` and `go/internal/mailparse` with [RFC 8620](https://www.rfc-editor.org/rfc/rfc8620.html) (JMAP Core) and [RFC 8621](https://www.rfc-editor.org/rfc/rfc8621.html) (JMAP Mail).

The review had two passes. The first found F1–F6. An independent follow-up assessment corrected F2, F3, F5 and F6 and added A1–A11. This page merges both, keeping their ids. Requirements stay in [JMAP](../specs/jmap.md), and the advertised surface is in the [conformance matrix](../specs/jmap-conformance.md).

Limits of this review:
- Findings come from reading the source. Some were also reproduced with temporary Go test overlays that were not committed, as noted on each finding.
- `go test ./internal/jmap/... ./internal/mailparse/...` passes at this commit.
- No deployed conformance gate was evaluated.
- The review is not exhaustive. A method with no finding is not thereby proven conformant.

## Summary

The implementation follows both RFCs closely. Every method in the conformance matrix exists and is registered under the right capability, and the core sync model is sound. That model covers the state tokens, `/changes` coalescing, `/queryChanges`, result references and per-object `/set` commits. The defects are mostly at the edges: input validation, response shape, header decoding, and one crash.

| Severity | Findings |
| --- | --- |
| High: crash from inbound mail | [F1](#f1-nested-multipartalternative-panics-the-body-list-algorithm) |
| Medium: wrong behaviour clients can see | [F2](#f2-no-emaildelivery-push-type), [F3](#f3-query-limit-not-reported-when-the-server-caps-results), [F4](#f4-from-to-and-subject-sorts-use-the-wrong-keys), [A1](#a1-eventsource-closeafterstate-stays-open-after-the-initial-event), [A2](#a2-valid-header-based-email-creation-is-rejected), [A3](#a3-multipart-composition-discards-part-metadata), [A4](#a4-escaped-keyword-patches-are-rejected), [A5](#a5-unchanged-server-set-values-are-rejected-on-update), [A8](#a8-text-header-form-lacks-nfc-and-control-removal), [A9](#a9-unknown-transfer-encodings-do-not-set-isencodingproblem) |
| Low: validation or response shape | [A6](#a6-malformed-request-envelopes-are-accepted), [A7](#a7-mailbox-name-limit-counts-characters-not-octets), [A10](#a10-empty-set-success-fields-are-not-null), [A11](#a11-querychanges-rejects-maxchanges-0), [F5](#f5-header-forms-and-lenient-email-creation) (forbidden form fetch) |
| Permitted, but worth tightening | [F5](#f5-header-forms-and-lenient-email-creation) (lenient creation), [F6](#f6-minor-deviations) |

## High

### F1. Nested `multipart/alternative` panics the body-list algorithm

- **Where:** `go/internal/mailparse/bodies.go:54` and `:71`, called from `go/internal/jmap/email_get.go:263` and `go/internal/search/extract.go:43`.
- **RFC:** RFC 8621 §4.1.4, the `parseStructure` pseudo-code.
- **Problem:** The Go port follows the pseudo-code exactly. When `inAlternative` is set, the pseudo-code sets `htmlBody` or `textBody` to null and passes it into a nested `multipart/alternative`, whose branch then appends to it. In Go that is a nil pointer dereference.
- **Reproduced:** in both passes. This valid message panics `BodyLists()`:

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
  - `Email/parse` uses the same `buildEmail` call.
  - The search indexer calls `BodyLists()` too, so the message can also block indexing.
- **Fix direction:** Guard the alternative branch against nil lists. Add the message above as a `mailparse` test and a fuzz seed. Also fuzz `BodyLists`; today `FuzzParse` covers only `Parse`.

## Medium

### F2. No `EmailDelivery` push type

- **Where:** `go/internal/jmap/push.go:25`.
- **RFC:** RFC 8621 §1.5. Mail servers "MUST support pushing state changes for a type called `EmailDelivery`". Its state "MUST change whenever a new Email is added to the store", and SHOULD NOT change on other Email changes.
- **Problem:** `pushTypes` lists only `Email`, `Mailbox`, `Thread`, `Identity` and `EmailSubmission`. `types=EmailDelivery` is silently dropped, and `types=*` leaves it out.
- **Fix direction:** Add a sequence that advances on every commit that adds an Email: ingest, `Email/import` and `Email/set` create (composition is included). Expose it in the StateChange.

### F3. Query `limit` not reported when the server caps results

- **Where:** `go/internal/jmap/mailbox.go:520-548` (`page`).
- **RFC:** RFC 8620 §5.5. The response `limit` "is only returned if the server set a limit or used a different limit than that given in the request."
- **Reproduced:** in the follow-up.
- **Problem:** Results are capped at 1024.
  - With no `limit` in the request, the cap applies and `limit` is left out of the response. A client that also omits `calculateTotal` cannot tell the list was truncated.
  - The branch `*limit < capped` echoes `limit` whenever the request asks for 1024 or more. That includes exactly 1024, where the limit was not changed.
- **Fix direction:** Return `limit` exactly when the effective limit differs from the requested one, counting "no limit requested" as different.

### F4. `from`, `to` and `subject` sorts use the wrong keys

- **Where:** `go/internal/jmap/email_query.go:191` (`firstAddress`) and `:225`.
- **RFC:** RFC 8621 §4.4.2.
  - `from` and `to` sort by the first address's name, or by its email if the name is null or empty.
  - `subject` sorts by the base subject (RFC 5256 §2.1), which drops `Re:`, `Fwd:` and similar prefixes and trailers.
- **Problem:**
  - `from` and `to` always compare the lowercased email address.
  - `subject` compares the whole lowercased subject, so `Re: Foo` sorts under R.
  - The advertised `i;unicode-casemap` collation is approximated with `strings.ToLower`.
- **Fix direction:** Derive the display name and base subject into Email metadata. Compare them with a casemap collation.

### A1. EventSource `closeafter=state` stays open after the initial event

- **Where:** `go/internal/jmap/push.go:118` (initial event) and `:152` (the only close check).
- **RFC:** RFC 8620 §7.3: with `closeafter=state`, "The server MUST end the HTTP response after pushing a state event". The RFC does not require an initial event.
- **Reproduced:** in the follow-up.
- **Problem:** The stream always sends an initial state event, and only closes after a later change. This is a conformance mismatch, not a broken long poll: clients still get a response when something changes.
- **Fix direction:**
  - When `closeafter=state`, skip the initial event, wait for a relevant change, emit one state event and close.
  - Closing right after an initial event would make every reconnect return at once and cause tight polling. `closeafter=no` streams can keep the initial event.
  - Update `TestEventSource`, which currently expects both events in one `closeafter=state` response.
  - Clients still recover changes made between connections through normal `/changes` sync.

### A2. Valid header-based Email creation is rejected

- **Where:** `go/internal/jmap/compose.go:155` and `:203`.
- **RFC:** RFC 8621 §4.6. The server "MUST generate" `Message-ID` and `Date` only "if not included". The `asDate` form is valid for `Date` (§4.1.2.6).
- **Reproduced:** in the follow-up for `Date`. The same code path exists for `Message-ID`.
- **Problem:** Automatic `Date` and `Message-ID` generation is skipped only for the exact keys `header:Date` and `header:Message-ID`. Two cases still collide with the generated header and return `invalidProperties` (`sentAt` conflicts with `header:Date:asDate`):
  - another valid form, such as `{"header:Date:asDate": "2026-10-17T06:00:00Z"}`;
  - a different capitalisation, or a `headers` entry for the same field.
- **Fix direction:** Generate the header only when no property of any form or capitalisation names that field.

### A3. Multipart composition discards part metadata

- **Where:** `go/internal/jmap/compose.go:538`, the multipart branch of `part()`.
- **Reproduced:** in the follow-up.
- **Problem:** The multipart branch returns before part metadata and custom headers are processed. A multipart part was created with `disposition: attachment`, a `cid` and `header:X-Test`. It was accepted, but the composed part kept only the generated `Content-Type`. The same branch also accepts and drops `Content-Transfer-Encoding` instead of rejecting it as leaf parts do.
- **Assessment:** §4.6 lets a server repair prohibited input, so dropping `Content-Transfer-Encoding` may be acceptable. Silently losing valid metadata is the defect.
- **Fix direction:** Apply the common part-header handling to multipart parts too.

### A4. Escaped keyword patches are rejected

- **Where:** `go/internal/jmap/email_set.go:187`.
- **RFC:** RFC 8620 §5.3 (PatchObject paths are JSON Pointers) and RFC 8621 §4.1.1 (`/` is allowed in a keyword).
- **Reproduced:** in the follow-up.
- **Problem:** `{"keywords/a~1b": null}` fails with `invalidPatch` ("nested keyword path"), even though `a/b` is a valid keyword that the whole `keywords` property can store. The code unescapes `~1` to `/` before it tests the key for a `/` separator.
- **Fix direction:** Check for nesting on the raw path, then unescape.

### A5. Unchanged server-set values are rejected on update

- **Where:** `applyPatch` in `go/internal/jmap/email_set.go`, and `parseMailboxPatch` in `go/internal/jmap/mailbox.go`.
- **RFC:** RFC 8620 §5.3: "Any server-set properties MAY be included in the patch if their value is identical to the current server value."
- **Reproduced:** in the follow-up for Email. The Mailbox case was confirmed in source.
- **Problem:** An update that includes the Email's own `id` returns `invalidProperties`. Mailbox updates reject `id`, the counts and `myRights` without comparing them to current values. This breaks clients that send whole objects back.
- **Fix direction:** Accept a server-set property when its value equals the current one, and reject it only when it differs.

### A8. Text header form lacks NFC and control removal

- **Where:** `go/internal/mailparse/header.go:97` (`AsText`).
- **RFC:** RFC 8621 §4.1.2.2. The decoded text is converted to NFC, and "Any NUL octets or control characters encoded per [RFC2047] are dropped".
- **Reproduced:** in the follow-up.
- **Problem:** Decomposed `e` + U+0301 stays decomposed. The encoded word `=?utf-8?Q?hi=00=01?=` yields NUL and U+0001. `AsText` backs `subject`, `header:*:asText` and display names, so control characters reach clients.
- **Fix direction:** Strip the control characters from decoded words, then apply NFC.

### A9. Unknown transfer encodings do not set `isEncodingProblem`

- **Where:** `decodeTransfer` in `go/internal/mailparse/structure.go:174`, and `Value` in `go/internal/mailparse/bodies.go`.
- **RFC:** RFC 8621 §4.1.4. `isEncodingProblem` is also set when "the content-transfer-encoding was unknown".
- **Reproduced:** in the follow-up.
- **Problem:** An unknown encoding passes the body through unchanged, and nothing records that fact. A text part with `Content-Transfer-Encoding: x-unknown` returns `isEncodingProblem: false`.
- **Fix direction:** Record the decoding status on the part and include it in the flag.

## Low

### A6. Malformed request envelopes are accepted

- **Where:** `Invocation.UnmarshalJSON` and `validate` in `go/internal/jmap/protocol.go:159`.
- **RFC:** RFC 8620 §3.3 (`using` is a required `String[]`) and §3.2 (the call id is a `String`). A request that does not match these types should get `notRequest`.
- **Reproduced:** in the follow-up.
- **Problem:** These requests return HTTP 200:
  - `{"methodCalls": []}`
  - `{"using": null, "methodCalls": []}`
  - a call with a `null` call id, which comes back as `""`
- **Fix direction:** Require `using`, and require string method names and call ids.

### A7. Mailbox name limit counts characters, not octets

- **Where:** `validMailboxName` in `go/internal/jmap/mailbox.go:639`.
- **RFC:** RFC 8621 §1.3.1. `maxSizeMailboxName` is measured "in (UTF-8) octets".
- **Reproduced:** in the follow-up.
- **Problem:** The validator counts code points. Eighty-six `界` characters (258 bytes) pass against the advertised limit of 255.
- **Fix direction:** Compare `len(name)` instead.

### A10. Empty `/set` success fields are not null

- **Where:** `newSetResponse` in `go/internal/jmap/mailbox.go:587`, which every `/set` handler uses.
- **RFC:** RFC 8620 §5.3. `created` and `updated` are "null if no Foo objects were successfully" created or updated, and `destroyed` is a list "or null if none".
- **Reproduced:** in the follow-up.
- **Problem:** An empty `Email/set` returns `created: {}`, `updated: {}` and `destroyed: []`. Commit `d62bc03` made the `not*` maps null, but not these three fields.
- **Fix direction:** Apply the same null-when-empty marshalling to these three fields.

### A11. `/queryChanges` rejects `maxChanges: 0`

- **Where:** `queryChangesResult` in `go/internal/jmap/changes.go`.
- **RFC:** RFC 8620 §5.6 types `maxChanges` as `UnsignedInt|null`. The "MUST be a positive integer greater than 0" rule appears only in §5.2, for `/changes`.
- **Reproduced:** in the follow-up.
- **Problem:** `maxChanges: 0` with no changes returns `invalidArguments` instead of an empty result.
- **Fix direction:** Accept 0. Return `tooManyChanges` only when the change count actually exceeds it.

### F5. Header forms and lenient Email creation

There are two parts here, and only the first is a definite defect.

**Forbidden forms on fetch.** RFC 8621 §4.2 says that fetching a form forbidden for a header field, such as `header:From:asDate`, "MUST result in the method call being rejected with an `invalidArguments` error". `parseHeaderProp` (`email_get.go:43`) and `validProps` accept any form for any header, in both `Email/get` and `Email/parse`.

**Lenient creation.** RFC 8621 §4.6 says creation attempts that break its rules "SHOULD be rejected with an `invalidProperties` error". It adds that a server "MAY choose to modify the Email" to comply instead. So the following leniencies are allowed, provided the resulting message complies:

- Accepting `headers` on the Email or on a part (`compose.go:62`).
- Accepting a forbidden form on create, such as `header:Subject:asAddresses`.
- Not checking that a `bodyStructure` part repeats a header already defined on the Email.
- Ignoring `size` when `partId` is given.

Header injection is still blocked by `validRawValue`. Rejecting these inputs would surface client bugs earlier.

These rules are enforced correctly: `Content-*` is refused on the Email, `charset` is refused with `partId`, truncated or encoding-problem `bodyValues` are refused, `bodyStructure` cannot be combined with the shortcut properties, and duplicate header properties are rejected. `Content-Transfer-Encoding` is rejected on leaf parts only; see [A3](#a3-multipart-composition-discards-part-metadata).

### F6. Minor deviations

| Area | Code | RFC | Observation |
| --- | --- | --- | --- |
| `/get` with `ids: null` | `mailbox.go:165` | 8620 §5.1 | `Mailbox/get` returns every Mailbox even beyond `maxObjectsInGet` (64), where the RFC allows "all" only within that limit. `Email/get` and `Thread/get` handle this correctly |
| `/changes` `maxChanges` | `changes.go:112` | 8620 §5.2 | When one commit holds more changes than `maxChanges`, the server returns `cannotCalculateChanges` and forces a full resync. Allowed, but costly; rare because commits are small |
| SetError types | `mailbox.go:606`, `submission.go:433` | 8620 §5.3 | `serverFail` (busy or commit failure) and `invalidArguments` (creation is not an object) are not SetError types. See the note below the table |
| Mailbox create response | `mailbox.go:829` | 8620 §5.3 | `created` omits the defaulted `parentId` and `role` (both null) when the client left them out |
| `Email/parse` defaults | `email_import.go:236` | 8621 §4.9 | The default property list adds `blobId` and `size`. Harmless extra data |
| EventSource ping | `push.go:158` | 8620 §7.3 | Pings are timed from the previous ping, not the previous event, on a 3 s poll tick. They can be early (harmless) or up to 3 s late |
| Problem types | `http.go`, `push.go` | 8620 §3.6.1 | `forbidden`, `notFound`, `serverFail` and `invalidArguments` are sent as request-level problem types, which RFC 8620 does not register. The HTTP statuses are still correct |
| Download caching | `http.go:184` | 8620 §6.2 | The RFC recommends long cache times with `immutable`; the code sends `no-store`. This is a deliberate security choice ([security](../specs/security.md)) |

**SetError types:** `invalidProperties` fits the "creation is not an object" case. Fixing the busy and commit-failure case is harder, because earlier objects in the same `/set` may already be committed. A method-level `serverFail` is only correct when nothing was changed. Otherwise the per-object result must stay accurate, as RFC 8620 §3.6.2 explains with `serverPartialFail`. `rateLimit` is not a general replacement either: it is defined for creation only.

## Verified as conforming

Checked against the RFC text, apart from the findings above:

- **Session (8620 §2, 8621 §1.3):**
  - All required core limits and `collationAlgorithms` are present.
  - The mail and submission capabilities are empty objects at session level and carry the required account-level properties.
  - The URL templates contain all required variables.
  - `sessionState` matches the session's `state`.
- **Request processing (8620 §3):**
  - `notJSON`, `unknownCapability` and `limit` are checked before any method runs. `notRequest` is only partly enforced; see [A6](#a6-malformed-request-envelopes-are-accepted).
  - A method whose capability is missing from `using` gets `unknownMethod`.
  - `createdIds` is echoed only when the request sent it.
  - Unknown method arguments are `invalidArguments`.
- **Result references (8620 §3.7):**
  - Lookup uses the first response with the call id, then checks the name.
  - JSON Pointer supports `*` and flattens arrays.
  - A `foo`/`#foo` conflict is `invalidArguments`.
- **`/changes` (8620 §5.2):**
  - Creates, updates and destroys are coalesced, and created-then-destroyed ids are dropped.
  - Pages end at commit boundaries with `hasMoreChanges`.
  - `maxChanges ≤ 0` is `invalidArguments`, as §5.2 requires.
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
  - Default `properties` and `bodyProperties` match the RFC lists.
  - The `header:{name}[:as{form}][:all]` grammar is parsed and the last instance wins.
  - `bodyValues` turns CRLF into LF and truncates on a character boundary. Charset problems set `isEncodingProblem`.
  - Every `Email/query` filter condition except `header` follows the RFC's inclusive/exclusive boundaries.
  - `collapseThreads` keeps the first result of each thread.
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
  - Upload returns 201 with `accountId`, `blobId`, `type` and `size`, and uploads are kept 24 hours.
  - Download honours `type` and `name`.
  - `Blob/copy` and `Email/copy` report `fromAccountNotFound` on a single-account server.
- **EventSource (8620 §7.3):**
  - `types` and `ping` are supported. The ping interval is clamped to 300 s and reported in `interval`.
  - The `StateChange` shape is correct.
  - `closeafter=no` behaves as specified; see [A1](#a1-eventsource-closeafterstate-stays-open-after-the-initial-event) for `state`.

## Out of scope or optional

- **`PushSubscription/*` (8620 §7.2):** Not implemented. §7 describes two push mechanisms but requires neither specifically. EventSource covers aerc and the planned webmail.
- **`VacationResponse` (8621 §8):** Not implemented. It has its own capability, so leaving it unadvertised conforms.
- **`header` filter condition (8621 §4.4.1):** Returns `unsupportedFilter`, as RFC 8620 §5.5 allows.
- **Multiple accounts:** Not supported. `/copy` methods are single-account by design.
- **WebSocket (RFC 8887):** Not advertised.

## Suggested order

1. **[F1](#f1-nested-multipartalternative-panics-the-body-list-algorithm):** Fix the nil-list guard and add regression and fuzz coverage. Any inbound mail can trigger it.
2. **Small, contained fixes:**
   - [F3](#f3-query-limit-not-reported-when-the-server-caps-results), [A4](#a4-escaped-keyword-patches-are-rejected), [A7](#a7-mailbox-name-limit-counts-characters-not-octets), [A10](#a10-empty-set-success-fields-are-not-null), [A11](#a11-querychanges-rejects-maxchanges-0) and [A6](#a6-malformed-request-envelopes-are-accepted).
   - [A1](#a1-eventsource-closeafterstate-stays-open-after-the-initial-event), with the `TestEventSource` update.
3. **Composition and header handling:** [A2](#a2-valid-header-based-email-creation-is-rejected), [A3](#a3-multipart-composition-discards-part-metadata), [A5](#a5-unchanged-server-set-values-are-rejected-on-update), [A8](#a8-text-header-form-lacks-nfc-and-control-removal), [A9](#a9-unknown-transfer-encodings-do-not-set-isencodingproblem), and the forbidden-form half of [F5](#f5-header-forms-and-lenient-email-creation).
4. **New state and sorting:**
   - [F2](#f2-no-emaildelivery-push-type): `EmailDelivery` state. Once it exists, clients can alert only on genuinely new mail.
   - [F4](#f4-from-to-and-subject-sorts-use-the-wrong-keys): sort keys, which need new metadata.
5. **Docs:** Update the [conformance matrix](../specs/jmap-conformance.md) in the same change as each fix.
