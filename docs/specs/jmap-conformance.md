# JMAP conformance matrix

[Documentation index](../README.md) · [JMAP](jmap.md#core-capabilities-to-implement) · [Security](security.md#scopes)

**Status:** lists what `go/internal/jmap` implements now; the [Phase 0 aerc evidence](../evidence/phase0-aerc-slice.md) records the deployed checks. Requirements stay in [JMAP](jmap.md), [storage and sync](storage-sync.md) and [security](security.md). Update this matrix in the same change as any method, limit or scope. Tests are in `go/internal/jmap/jmap_test.go` unless named otherwise.

## Advertised session

| Item | Value |
| --- | --- |
| Capabilities | `urn:ietf:params:jmap:core`, `urn:ietf:params:jmap:mail`, and `urn:ietf:params:jmap:submission` when sending is configured (`SEND_ALIASES`). No WebSocket or custom capability yet |
| Submission limits | `maxDelayedSend` 0; `submissionExtensions` empty; 100 envelope recipients; 20 MiB message |
| Core limits | `maxSizeUpload` 4,194,304; `maxConcurrentUpload` 4; `maxSizeRequest` 1,048,576; `maxConcurrentRequests` 4; `maxCallsInRequest` 16; `maxObjectsInGet` 64; `maxObjectsInSet` 16 |
| Mail limits | `maxMailboxesPerEmail` 8; `maxMailboxDepth` 10; `maxSizeMailboxName` 255; `maxSizeAttachmentsPerEmail` 20 MiB; `mayCreateTopLevelMailbox` true |
| Account id | `A` + base32 of a hash of the internal account and restore epoch: a restored account gets a new id (RFC 8620 §1.6.2) |
| State tokens | `<epoch>-<seq>`, where `seq` is the last commit that changed the type (STATE `emailState`, `mailboxState`, `threadState`, `identityState`, `submissionState`); `/changes` pages end at commit boundaries; a foreign epoch or future sequence is `cannotCalculateChanges` |
| Query states | `q<epoch>-<seq>-<hash>`, where the hash covers the normalized filter, sort and query options; another query's state is `cannotCalculateChanges` |
| `uploadUrl` | `POST /upload/{accountId}`; see [blobs](#blobs-and-push) |

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
| `Mailbox/queryChanges` | 8621 §2.4 | read | Every mailbox created, updated or destroyed since the query state is removed and, if still a result, re-added at its index; tree modes include descendants of changed mailboxes; counter-only changes are ignored; `maxChanges` → `tooManyChanges`; `upToId` ignored | Index-backed filters → `cannotCalculateChanges` | `TestQueryChanges`, `TestTextQueryAndSnippets` |
| `Mailbox/set` | 8621 §2.5 | write | `ifInState`; create (no role), update (`name`, `parentId`, `sortOrder`, `isSubscribed`) and destroy, one guarded commit each; creation references in `parentId`, update keys and destroy; sibling-name uniqueness, cycle and depth (10) checks; system mailboxes can be renamed, re-sorted and (un)subscribed but not moved, re-roled or destroyed (`forbidden`); `mailboxHasChild`; `mailboxHasEmail` unless `onDestroyRemoveEmails`, which empties the mailbox one Email commit at a time (Emails in no other mailbox are destroyed) within a 15 s budget | — | `TestMailboxSetTree` |
| `Thread/get` | 8621 §3.1 | read | Members oldest first from MEMBER records; threads assigned at creation by Message-ID/In-Reply-To/References, then reply subject + shared address within 30 days, never renumbered ([storage](storage-sync.md#account-model)); `ids: null` is `requestTooLarge` | — | `TestMailboxQuerySetAndThreads`, `TestEmailDestroyAndThreads`, `store` component tests |
| `Thread/changes` | 8621 §3.2 | read | Thread created, updated (membership) and destroyed per commit | — | `TestEmailSetAndChanges` |
| `Email/get` | 8621 §4.2 | read | Metadata properties from EMAIL; header, body and `header:*` forms parsed from the raw original by exact version; `bodyStructure`, `textBody`/`htmlBody`/`attachments`, `bodyValues` with `maxBodyValueBytes`, `preview` | — | `TestEmailGetBody`, `TestQueryThenGetByReference` |
| `Email/changes` | 8621 §4.3 | read | Coalesced created/updated/destroyed with `maxChanges` paging at commit boundaries | — | `TestEmailSetAndChanges`, `TestChangesCoalesce` |
| `Email/query` | 8621 §4.4 | read | Filters `inMailbox`, `inMailboxOtherThan`, `before`, `after`, `minSize`, `maxSize`, `hasKeyword`, `notKeyword`, the three thread-keyword filters over all thread members, `from`, `to`, `cc`, `subject` (metadata substring), `text`, `body`, `bcc` and `hasAttachment` (search index; candidates first, then live metadata) and the AND/OR/NOT operators; sorts as advertised (thread-keyword sorts over all members); `position`, `anchor`, `anchorOffset`, `limit` (cap 1024), `calculateTotal`; `collapseThreads` keeps each thread's first result | Index-backed queries report `canCalculateChanges: false` and their query state includes the index generation; a missing or lagging index is `serverUnavailable` ([search](search.md#jmap-exposure)) | `TestEmailQueryOptions`, `TestTextQueryAndSnippets` |
| `Email/queryChanges` | 8621 §4.5 | read | Every Email changed since the query state is removed and, if still a result, re-added at its index; thread-dependent queries (`collapseThreads`, thread-keyword filters or sorts) include every member of a touched thread; `maxChanges` → `tooManyChanges`; `upToId` ignored | Index-backed filters → `cannotCalculateChanges` | `TestQueryChanges`, `TestTextQueryAndSnippets` |
| `Email/set` | 8621 §4.6 | write | `ifInState`; updates to `keywords`, `keywords/*`, `mailboxIds`, `mailboxIds/*` with validation (at least one mailbox, at most 8, known ids, valid keywords), creation references in ids and `mailboxIds`, one guarded transaction per Email; `destroy` removes the Email and its derived records, writes a deletion marker and never deletes the original; `create` composes MIME from header properties (including `header:*` in every form but `asGroupedAddresses`), `bodyStructure` or `textBody`/`htmlBody`/`attachments` with `bodyValues` and blob parts (inline `cid` attachments go into `multipart/related`), generates `Date` and `Message-ID` when absent, archives it under `composed/` before committing and threads it like a delivery. Rejected: server-set properties, `Content-*` at Email level, `Content-Transfer-Encoding` on parts, `charset` on `bodyValues` parts, unreferenced `bodyValues`, more than 64 parts or 20 MiB | — | `TestEmailSetAndChanges`, `TestEmailDestroyAndThreads`, `TestEmailCreate` (`blob_test.go`) |
| `Email/import` | 8621 §4.8 | write | `ifInState`; from an upload or any readable blob; archives the exact bytes under `imported/` before the commit; `blobNotFound`, `invalidEmail`, `invalidProperties`, `tooLarge` (20 MiB) per object | — | `TestUploadImportParseAndDownload` |
| `Email/parse` | 8621 §4.9 | read | Header and body properties of any readable message blob, including attached messages; object-only properties are null; part blob ids extend the parsed blob's part path | — | `TestUploadImportParseAndDownload` |
| `Email/copy` | 8621 §4.7 | write | Single account: the same account is `invalidArguments`, any other `fromAccountNotFound` | — | `TestCopyMethods` |
| `Blob/copy` | 8620 §6.3 | write | As `Email/copy` | — | `TestCopyMethods` |
| `SearchSnippet/get` | 8621 §5.1 | read | Unknown or destroyed ids are `notFound` before any text is read; `subject` and `preview` are HTML-escaped with only `<mark>` inserted, null without a match | Terms come from `text`, `body` and `subject` conditions, including negated ones | `TestTextQueryAndSnippets` |
| `Identity/get` | 8621 §6.1 | read | One identity per declared sending alias (the receiving aliases minus the canary), reconciled with the registry by a guarded commit on each Identity or submission call; deterministic ids; `mayDelete` false | — | `TestIdentities` (`submission_test.go`) |
| `Identity/changes` | 8621 §6.2 | read | Created/destroyed by alias changes, updated by `Identity/set` | — | `TestIdentities` |
| `Identity/set` | 8621 §6.3 | write | `ifInState`; update `name`, `replyTo`, `bcc`, `textSignature`, `htmlSignature` (bounded sizes, valid addresses); `email` is immutable; create and destroy are `forbidden` because identities follow the alias declaration | — | `TestIdentities` |
| `EmailSubmission/get` | 8621 §7.1 | read | All properties; `undoStatus` from the send state (pending while queued or claimed before handoff); `deliveryStatus` per envelope recipient: `queued` while sending and after relay acceptance until a delivery report arrives, then `yes` (delivered, or a spam complaint) or `no` (hard bounce or relay reject; a soft bounce keeps `queued`); `no` after rejection or cancellation; `unknown` after uncertain acceptance; reports never undo `yes` or `no`; `displayed` always `unknown`; `dsnBlobIds`/`mdnBlobIds` empty; destroyed submissions are hidden; `ids: null` beyond 64 is `requestTooLarge` | Suppression lists are not shown | `TestAercSendFlow` |
| `EmailSubmission/changes` | 8621 §7.2 | read | Created, updated (every send-state change) and destroyed | — | `TestAercSendFlow` |
| `EmailSubmission/query` | 8621 §7.3 | read | Filters `identityIds`, `emailIds`, `threadIds`, `undoStatus`, `before`, `after` and operators; sorts `emailId`, `threadId`, `sentAt` | — | `TestAercSendFlow`, `TestSubmissionCancelAfterHandoffAndQueryChanges` |
| `EmailSubmission/queryChanges` | 8621 §7.4 | read | Every submission changed since the query state is removed and, if still a result, re-added | — | `TestSubmissionCancelAfterHandoffAndQueryChanges` |
| `EmailSubmission/set` | 8621 §7.5 | submit (+ write for hooks) | `ifInState`; create with `identityId`, `emailId` (creation references allowed) and an optional envelope; `From` (and `Sender`) must be exactly the identity, `MAIL FROM` must be the identity, no SMTP parameters; without an envelope the recipients are To/Cc/Bcc; one Message-ID and a Date are required. The commit holds the submission (pinning the Email's exact original version), the daily and monthly quota reservation, the first attempt record and the dispatch outbox. Errors: `invalidProperties` (unknown identity or Email, server-set properties including `sendAt`), `forbiddenFrom`, `forbiddenMailFrom`, `invalidEmail`, `noRecipients`, `invalidRecipients`, `tooManyRecipients`, `tooLarge`, `overQuota`. Update `undoStatus: canceled` before handoff (else `cannotUnsend`), releasing the quota; destroy hides the submission without canceling it. `onSuccessUpdateEmail`/`onSuccessDestroyEmail` run as an implicit `Email/set` response with the same call id and need the write scope, checked before anything is created | — | `TestAercSendFlow`, `TestSubmissionCreateWithComposedEmail`, `TestSubmissionCreateErrors` |

Not advertised or served yet: `VacationResponse/*`.

## Blobs and push

| Route | Behavior | Test |
| --- | --- | --- |
| `GET /download/{accountId}/{blobId}/{name}` | Raw message (`G…`) or part (`P…-<path>`) of a live Email, or an unexpired upload (`U…`) or part of an uploaded message (`V…-<path>`), in the caller's account; a path such as `2.1` descends into attached messages; anything else 404. Always `attachment` disposition with an RFC 8187 filename, `nosniff`, `Content-Security-Policy: sandbox`, `no-store` | `TestDownload` |
| `GET /eventsource` (RFC 8620 §7.3) | `types` (`*` or Email, Mailbox, Thread, Identity, EmailSubmission), `closeafter`, `ping` up to 300 s; polls STATE every 3 s; comment keepalive after 60 s quiet; ends at token expiry, 14 min 30 s, or 15 s before the Lambda deadline | `TestEventSource` |
| `POST /upload/{accountId}` (RFC 8620 §6.1) | `mail.write`; at most `maxSizeUpload` (413 `limit` problem); the body reaches Lambda base64-encoded (`binary_media_types` `*/*`); stored in the disposable blob bucket with a BLOB record; usable for 24 hours, then 404/`blobNotFound` | `TestUploadImportParseAndDownload`, `TestUploadLimitsAndScope` |

Push connection leases ([JMAP push](jmap.md#push)) are not implemented; the push function's reserved concurrency (4) caps open streams instead.
