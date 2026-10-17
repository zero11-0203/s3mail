# JMAP conformance matrix

[Documentation index](../README.md) · [JMAP](jmap.md#core-capabilities-to-implement) · [Security](security.md#scopes)

**Status:** lists what `go/internal/jmap` implements now; the [Phase 0 aerc evidence](../evidence/phase0-aerc-slice.md) records the deployed checks. Requirements stay in [JMAP](jmap.md), [storage and sync](storage-sync.md) and [security](security.md). Update this matrix in the same change as any method, limit or scope. Tests are in `go/internal/jmap/jmap_test.go` unless named otherwise.

## Advertised session

| Item | Value |
| --- | --- |
| Capabilities | `urn:ietf:params:jmap:core`, `urn:ietf:params:jmap:mail`. No submission, WebSocket or custom capability yet |
| Core limits | `maxSizeUpload` 4,194,304; `maxConcurrentUpload` 4; `maxSizeRequest` 1,048,576; `maxConcurrentRequests` 4; `maxCallsInRequest` 16; `maxObjectsInGet` 64; `maxObjectsInSet` 16 |
| Mail limits | `maxMailboxesPerEmail` 8; `maxMailboxDepth` 10; `maxSizeMailboxName` 255; `maxSizeAttachmentsPerEmail` 20 MiB; `mayCreateTopLevelMailbox` true |
| Account id | `A` + base32 of a hash of the internal account and restore epoch: a restored account gets a new id (RFC 8620 §1.6.2) |
| State tokens | `<epoch>-<seq>`, where `seq` is the last commit that changed the type (STATE `emailState`, `mailboxState`, `threadState`); `/changes` pages end at commit boundaries; a foreign epoch or future sequence is `cannotCalculateChanges` |
| Query states | `q<epoch>-<seq>-<hash>`, where the hash covers the normalized filter, sort and query options; another query's state is `cannotCalculateChanges` |
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
| `Mailbox/get` | 8621 §2.1 | read | Seven system mailboxes (Quarantine has no role) merged with stored overrides and custom mailboxes; counts from the transactional COUNT records; `myRights` (system mailboxes: `mayDelete` false) | — | `TestEmailSetAndChanges`, `TestMailboxSetTree` |
| `Mailbox/changes` | 8621 §2.2 | read | Exact per-mailbox changes; `updatedProperties` lists the four counts when the window holds only counter changes, else null | — | `TestEmailSetAndChanges` |
| `Mailbox/query` | 8621 §2.3 | read | Filters `parentId`, `name`, `role`, `hasAnyRole`, `isSubscribed` and operators; sorts `sortOrder`, `name`; `sortAsTree`, `filterAsTree` | — | `TestMailboxQuerySetAndThreads`, `TestMailboxSetTree` |
| `Mailbox/queryChanges` | 8621 §2.4 | read | Every mailbox created, updated or destroyed since the query state is removed and, if still a result, re-added at its index; tree modes include descendants of changed mailboxes; counter-only changes are ignored; `maxChanges` → `tooManyChanges`; `upToId` ignored | — | `TestQueryChanges` |
| `Mailbox/set` | 8621 §2.5 | write | `ifInState`; create (no role), update (`name`, `parentId`, `sortOrder`, `isSubscribed`) and destroy, one guarded commit each; creation references in `parentId`, update keys and destroy; sibling-name uniqueness, cycle and depth (10) checks; system mailboxes can be renamed, re-sorted and (un)subscribed but not moved, re-roled or destroyed (`forbidden`); `mailboxHasChild`; `mailboxHasEmail` unless `onDestroyRemoveEmails`, which empties the mailbox one Email commit at a time (Emails in no other mailbox are destroyed) within a 15 s budget | — | `TestMailboxSetTree` |
| `Thread/get` | 8621 §3.1 | read | Members oldest first from MEMBER records; threads assigned at creation by Message-ID/In-Reply-To/References, then reply subject + shared address within 30 days, never renumbered ([storage](storage-sync.md#account-model)); `ids: null` is `requestTooLarge` | — | `TestMailboxQuerySetAndThreads`, `TestEmailDestroyAndThreads`, `store` component tests |
| `Thread/changes` | 8621 §3.2 | read | Thread created, updated (membership) and destroyed per commit | — | `TestEmailSetAndChanges` |
| `Email/get` | 8621 §4.2 | read | Metadata properties from EMAIL; header, body and `header:*` forms parsed from the raw original by exact version; `bodyStructure`, `textBody`/`htmlBody`/`attachments`, `bodyValues` with `maxBodyValueBytes`, `preview` | — | `TestEmailGetBody`, `TestQueryThenGetByReference` |
| `Email/changes` | 8621 §4.3 | read | Coalesced created/updated/destroyed with `maxChanges` paging at commit boundaries | — | `TestEmailSetAndChanges`, `TestChangesCoalesce` |
| `Email/query` | 8621 §4.4 | read | Filters `inMailbox`, `inMailboxOtherThan`, `before`, `after`, `minSize`, `maxSize`, `hasKeyword`, `notKeyword`, the three thread-keyword filters over all thread members, `from`, `to`, `cc`, `subject` and the AND/OR/NOT operators; sorts as advertised (thread-keyword sorts over all members); `position`, `anchor`, `anchorOffset`, `limit` (cap 1024), `calculateTotal`; `collapseThreads` keeps each thread's first result | `text`, `body` and other search filters (`unsupportedFilter`) until [search](search.md) | `TestEmailQueryOptions` |
| `Email/queryChanges` | 8621 §4.5 | read | Every Email changed since the query state is removed and, if still a result, re-added at its index; thread-dependent queries (`collapseThreads`, thread-keyword filters or sorts) include every member of a touched thread; `maxChanges` → `tooManyChanges`; `upToId` ignored | — | `TestQueryChanges` |
| `Email/set` | 8621 §4.6 | write | `ifInState`; updates to `keywords`, `keywords/*`, `mailboxIds`, `mailboxIds/*` with validation (at least one mailbox, at most 8, known ids, valid keywords), creation references in ids and `mailboxIds`, one guarded transaction per Email; `destroy` removes the Email and its derived records, writes a deletion marker and never deletes the original | `create` (refused per object with `forbidden`) | `TestEmailSetAndChanges`, `TestEmailDestroyAndThreads` |

Not advertised or served yet: `Identity/*`, `EmailSubmission/*`, `SearchSnippet/get`, `Email/copy`, `Email/import`, `Email/parse`, `Blob/copy`, `VacationResponse/*`.

## Blobs and push

| Route | Behavior | Test |
| --- | --- | --- |
| `GET /download/{accountId}/{blobId}/{name}` | Raw message (`G…`) or one leaf part (`P…-<partId>`) of a live Email in the caller's account; anything else 404. Always `attachment` disposition with an RFC 8187 filename, `nosniff`, `Content-Security-Policy: sandbox`, `no-store` | `TestDownload` |
| `GET /eventsource` (RFC 8620 §7.3) | `types` (`*` or Email, Mailbox, Thread), `closeafter`, `ping` up to 300 s; polls STATE every 3 s; comment keepalive after 60 s quiet; ends at token expiry, 14 min 30 s, or 15 s before the Lambda deadline | `TestEventSource` |
| `POST /upload/{accountId}` | Not served | — |

Push connection leases ([JMAP push](jmap.md#push)) are not implemented; the push function's reserved concurrency (4) caps open streams instead.
