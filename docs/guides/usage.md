# Using s3mail

[Documentation index](../README.md) · [Deploy guide](deploy.md) · [Costs](costs.md)

This guide is for day-to-day use after [deployment](deploy.md): signing in, connecting aerc, managing addresses, sending, and the few maintenance tasks s3mail needs.

## How s3mail organizes mail

- **One account, many addresses.** Every alias in `mail_aliases` delivers to the same mailbox. Plus-tags work: mail to `alice+shop@example.com` arrives if `alice` is declared. Mail to an undeclared address is rejected during the SMTP conversation, so the sender gets a bounce and nothing is stored.
- **Mailboxes.** Inbox, Archive, Drafts, Sent, Junk, Trash and **Quarantine** always exist, and you can create your own. Quarantine holds mail that failed the sender-authentication policy (for example a DMARC `reject` failure) or could not be parsed safely. Check it now and then and move legitimate messages to the Inbox.
- **Deleting hides; it does not erase.** Destroying a message removes it from mailboxes and search. The original stays permanently in the S3 archive ([retention](../operations/recovery.md#original-mail-retention)). Plan storage accordingly ([costs](costs.md)).
- **Search** is full-text, including Chinese, Japanese and Korean text. New and changed mail is usually searchable within about six minutes.
- **Protocol.** s3mail speaks [JMAP](https://jmap.io/) only, with no IMAP or SMTP submission, so you need a JMAP client. aerc is the tested one.

## Sign in

`mail-auth` gets OAuth tokens from Cognito in your browser and hands short-lived access tokens to your mail client. It runs on macOS only for now, because it stores the session in the Keychain.

Write its configuration from the Terraform outputs, then sign in:

```sh
mkdir -p ~/.config/s3mail
terraform -chdir=infra/main output -json auth > ~/.config/s3mail/auth.json
mail-auth login
```

`login` opens the Cognito sign-in page and listens on `http://localhost:53789/callback`. The first time, sign in with the temporary password from [deployment](deploy.md#7-create-your-user) and choose your real password.

| Command | What it does |
| --- | --- |
| `mail-auth token` | Prints a valid access token and refreshes it when needed. Mail clients call this. |
| `mail-auth status` | Shows whether you are signed in and when the access token expires |
| `mail-auth logout` | Revokes the refresh token and deletes the stored session |
| `mail-auth version` | Prints the build version |

Access tokens last five minutes. After `logout`, a token that was already issued keeps working until it expires.

## Set up aerc

Get the session URL:

```sh
terraform -chdir=infra/main output -json jmap | jq -r .session_url
# https://abc123.execute-api.us-east-1.amazonaws.com/v1/.well-known/jmap
```

Add an account to aerc's `accounts.conf`. On macOS the file is `~/Library/Preferences/aerc/accounts.conf`; on Linux it is `~/.config/aerc/accounts.conf`. It must be readable only by you (`chmod 600`).

```ini
[example]
source          = jmap+oauthbearer://abc123.execute-api.us-east-1.amazonaws.com/v1/.well-known/jmap
source-cred-cmd = mail-auth token
outgoing        = jmap://
from            = Alice <alice@example.com>
default         = Inbox
cache-state     = true
```

- `source` is the session URL with `https://` replaced by `jmap+oauthbearer://`.
- `outgoing = jmap://` sends through the same JMAP connection; see `aerc-jmap(5)`.
- `from` must be one of your aliases exactly; each alias is a JMAP Identity.
- `default = Inbox` must match the mailbox name. `INBOX` does not select it.

aerc opens a push connection, so new mail appears without polling.

### Other JMAP clients

Any client that supports JMAP with OAuth bearer tokens can work. It needs the full session URL above, because the API does not yet have a custom domain or the `_jmap._tcp` SRV record. A client without built-in OAuth can use `mail-auth token` if it accepts a token from a command. Only aerc is tested so far; reports for other clients are welcome.

### Browser webmail

If you deployed the optional [browser webmail](deploy.md#optional-browser-webmail), a pinned build of [`root-fr/jmap-webmail`](https://github.com/root-fr/jmap-webmail), open the address from `terraform -chdir=infra/webmail output -raw url` and sign in with the same Cognito user. It checks for new mail by polling instead of push. See [clients](../specs/clients.md#browser-hosting-and-client-safety) for how it is hosted and handles tokens.

## Addresses

To add or remove an address, edit `mail_aliases` in `infra/main/terraform.tfvars`, then plan and apply. The receiving rule and the matching sending identity update together. `canary` is reserved for deployment checks.

Each identity's display name, reply-to address and signature can be changed through JMAP `Identity/set`, in clients that offer it. The sending address of an identity cannot be changed; add a new alias instead.

## Sending limits and failed sends

- **Quota.** Every submission reserves capacity against `relay_quota` (by default 200 recipients a day and 1,000 a month, UTC). When it runs out, the send is refused with an error that explains why. Raise the quota only if your relay plan allows it.
- **Limits.** Up to 100 recipients and 20 MiB per message. Single attachment uploads are limited to 4 MiB by the current API settings.
- **Delivery status** comes from SMTP2GO's webhooks. Delivered, bounced and rejected recipients show in the client's submission status, and you can list them with `mail-admin`. It reads the table with your AWS credentials, not your mail password:

  ```sh
  TABLE=$(terraform -chdir=infra/main output -json mail_inbound | jq -r .table_name)
  mail-admin -table "$TABLE" submissions        # every submission and its state
  mail-admin -table "$TABLE" submission <id>    # one submission with its attempt history
  ```

- **Unknown acceptance.** If the connection to the relay breaks after the message was handed over, s3mail cannot know whether it was sent. It marks the submission `unknown` and **never resends it automatically**, because that could send twice. Check SMTP2GO's activity log, then record what happened:

  ```sh
  mail-admin -table "$TABLE" resolve <id> accepted   # it was sent
  mail-admin -table "$TABLE" resolve <id> rejected   # it was not sent; give up
  mail-admin -table "$TABLE" resolve <id> retry      # it was not sent; try again
  ```

- **Suppressed recipients.** SMTP2GO may block addresses after hard bounces or complaints, and those sends fail permanently. View and clear suppressions in the SMTP2GO dashboard.
- **Pause sending** without a deployment by setting the relay parameter to `paused` ([deploy guide](deploy.md#8-set-up-sending)). Queued mail waits and continues when you set it back to `smtp2go`.

## Routine checks

s3mail has no alarms by default, so problems show up only when you look.

- Open the dashboard (`terraform -chdir=infra/main output dashboard_url`) after changes and every so often. Any message in a dead-letter queue (DLQ) needs attention.
- After sending something important, check `mail-admin ... submissions` for `unknown` or rejected entries.
- Look in Quarantine.
- Watch the AWS bill; see [costs](costs.md) for what to expect.

If mailbox counts or thread listings ever look wrong, rebuild the derived records from the stored messages. This never touches originals or messages:

```sh
mail-admin -table "$TABLE" rebuild-derived
```

For deeper recovery, see the [recovery guide](../operations/recovery.md).

## If your password or device is compromised

1. Disable the user and revoke every session:

   ```sh
   POOL_ID=$(terraform -chdir=infra/main output -json auth | jq -r '.issuer | split("/") | last')
   aws cognito-idp admin-disable-user --user-pool-id "$POOL_ID" --username alice
   aws cognito-idp admin-user-global-sign-out --user-pool-id "$POOL_ID" --username alice
   ```

   Tokens that were already issued stay valid for up to five minutes.
2. Pause sending (above), and check `submissions` for mail you did not send.
3. Set a new password (`aws cognito-idp admin-set-user-password ... --permanent`), enable the user again with `admin-enable-user`, and sign in again with `mail-auth login`.

Anyone with administrator access to the AWS account can read all mail, so protect that account with MFA.

## Updating

Pull and apply new versions as described in the [deploy guide](deploy.md#updating). Clients keep working across code updates. If you rebuild `mail-auth`, run `go install` again.

## Current limitations

- macOS only for `mail-auth`.
- No vacation responder (`VacationResponse`), no IMAP, no SMTP submission for other clients.
- No alarms, budgets or backups by default.
- At most four push connections at once; an abandoned connection holds its slot for up to a minute.
- Messages over SES's 40 MB receiving limit are rejected by SES.
