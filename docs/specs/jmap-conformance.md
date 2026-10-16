# JMAP conformance matrix

[Documentation index](../README.md) · [JMAP](jmap.md#core-capabilities-to-implement) · [Security](security.md#scopes)

**Status:** lists what `go/internal/jmap` implements now; the [Phase 0 aerc evidence](../evidence/phase0-aerc-slice.md) records the deployed checks. Requirements stay in [JMAP](jmap.md), [storage and sync](storage-sync.md) and [security](security.md). Update this matrix in the same change as any method, limit or scope. Tests are in `go/internal/jmap/jmap_test.go` unless named otherwise.

## Advertised session

| Item | Value |
| --- | --- |
| Capabilities | `urn:ietf:params:jmap:core`, `urn:ietf:params:jmap:mail`. No submission, WebSocket or custom capability yet |
| Core limits | `maxSizeUpload` 4,194,304; `maxConcurrentUpload` 4; `maxSizeRequest` 1,048,576; `maxConcurrentRequests` 4; `maxCallsInRequest` 16; `maxObjectsInGet` 64; `maxObjectsInSet` 16 |
| Mail limits | `maxMailboxesPerEmail` 8; `maxMailboxDepth` 1; `maxSizeMailboxName` 255; `maxSizeAttachmentsPerEmail` 20 MiB; `mayCreateTopLevelMailbox` false |
| Account id | `A` + base32 of a hash of the internal account and restore epoch: a restored account gets a new id (RFC 8620 §1.6.2) |
| State tokens | `<epoch>-<seq>` from the account sequence, shared by every type; a foreign epoch or future sequence is `cannotCalculateChanges` |
| `uploadUrl` | Required in the session object but **not served yet**; uploads fail until upload is implemented |

## Request handling (RFC 8620 §3)

| Behavior | Implementation | Test |
| --- | --- | --- |
| Request errors §3.6.1 | `notJSON`, `notRequest`, `unknownCapability`, `limit` (`maxSizeRequest`, `maxCallsInRequest`) as problem JSON, checked before any method runs | `TestRequestErrors` |
| Method errors §3.6.2 | `unknownMethod` (also for a method whose capability is not in `using`), `invalidArguments` (unknown or ill-typed argument), `accountNotFound`, `forbidden`, `serverFail`, `serverUnavailable` | `TestMethodErrors` |
| Result references §3.7 | JSON Pointer with `*`; mismatched name or call id is `invalidResultReference` | `TestQueryThenGetByReference` |
| Consistent reads | Every read checks STATE before and after (3 tries, then `serverUnavailable`) | `TestEmailSetAndChanges` |
| Unmapped subject | 403 problem before any method runs | `TestRequestErrors` |

## Methods

Scope is the minimum wire scope; a call without it is `forbidden`. All calls are scoped to the caller's mapped account.

| Method | RFC | Scope | Coverage now | Not yet | Test |
| --- | --- | --- | --- | --- | --- |
| `Core/echo` | 8620 §4 | read | Full | — | `TestMethodErrors` |
| `Mailbox/get` | 8621 §2.1 | read | Seven fixed mailboxes with roles (Quarantine has none), counts from EMAIL records, `myRights` | — | `TestEmailSetAndChanges` |
| `Mailbox/changes` | 8621 §2.2 | read | Every mailbox reported updated when any Email changes, with `updatedProperties` limited to counts | Exact per-mailbox changes | `TestEmailSetAndChanges` |
| `Mailbox/query` | 8621 §2.3 | read | Filters `role`, `hasAnyRole`, `name`, `parentId` null; sorts `sortOrder`, `name` | `isSubscribed` (`unsupportedFilter`) | `TestMailboxQuerySetAndThreads` |
| `Mailbox/queryChanges` | 8621 §2.4 | read | `cannotCalculateChanges` | Query changes | `TestEmailQueryOptions` |
| `Mailbox/set` | 8621 §2.5 | write | `ifInState`; every create/update/destroy refused per object with `forbidden` | Mailbox mutation | `TestMailboxQuerySetAndThreads` |
| `Thread/get` | 8621 §3.1 | read | One Email per thread; `ids: null` is `requestTooLarge` | Threading by references | `TestMailboxQuerySetAndThreads` |
| `Thread/changes` | 8621 §3.2 | read | From Email creations | Membership changes | `TestEmailSetAndChanges` |
| `Email/get` | 8621 §4.2 | read | Metadata properties from EMAIL; header, body and `header:*` forms parsed from the raw original by exact version; `bodyStructure`, `textBody`/`htmlBody`/`attachments`, `bodyValues` with `maxBodyValueBytes`, `preview` | — | `TestEmailGetBody`, `TestQueryThenGetByReference` |
| `Email/changes` | 8621 §4.3 | read | Coalesced created/updated/destroyed with `maxChanges` paging at commit boundaries | — | `TestEmailSetAndChanges`, `TestChangesCoalesce` |
| `Email/query` | 8621 §4.4 | read | Filters `inMailbox`, `inMailboxOtherThan`, `before`, `after`, `minSize`, `maxSize`, `hasKeyword`, `notKeyword`, the three thread-keyword filters (equal to `hasKeyword` with 1:1 threads), `from`, `to`, `cc`, `subject` and the AND/OR/NOT operators; sorts as advertised; `position`, `anchor`, `anchorOffset`, `limit` (cap 1024), `calculateTotal`; `collapseThreads` is a no-op with 1:1 threads | `text`, `body` and other search filters (`unsupportedFilter`) until [search](search.md) | `TestEmailQueryOptions` |
| `Email/queryChanges` | 8621 §4.5 | read | `cannotCalculateChanges` | Query changes | `TestEmailQueryOptions` |
| `Email/set` | 8621 §4.6 | write | `ifInState`; updates to `keywords`, `keywords/*`, `mailboxIds`, `mailboxIds/*` with validation (at least one mailbox, at most 8, known ids, valid keywords), one guarded transaction per Email | `create` and `destroy` (refused per object with `forbidden`) | `TestEmailSetAndChanges` |

Not advertised or served yet: `Identity/*`, `EmailSubmission/*`, `SearchSnippet/get`, `Email/copy`, `Email/import`, `Email/parse`, `Blob/copy`, `VacationResponse/*`.

## Blobs and push

| Route | Behavior | Test |
| --- | --- | --- |
| `GET /download/{accountId}/{blobId}/{name}` | Raw message (`G…`) or one leaf part (`P…-<partId>`) of a live Email in the caller's account; anything else 404. Always `attachment` disposition with an RFC 8187 filename, `nosniff`, `Content-Security-Policy: sandbox`, `no-store` | `TestDownload` |
| `GET /eventsource` (RFC 8620 §7.3) | `types` (`*` or Email, Mailbox, Thread), `closeafter`, `ping` up to 300 s; polls STATE every 3 s; comment keepalive after 60 s quiet; ends at token expiry, 14 min 30 s, or 15 s before the Lambda deadline | `TestEventSource` |
| `POST /upload/{accountId}` | Not served | — |

Push connection leases ([JMAP push](jmap.md#push)) are not implemented; the push function's reserved concurrency (4) caps open streams instead.
