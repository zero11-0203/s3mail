# Deploying s3mail

[Documentation index](../README.md) · [Usage guide](usage.md) · [Costs](costs.md)

This guide deploys s3mail into your own AWS account with the tooling that exists today: `make`, Terraform and the AWS CLI, run from your computer. Several steps are manual.

## What you get

- Mail for `<alias>@<your domain>` received by Amazon SES and stored permanently in S3.
- A JMAP server (API Gateway and Lambda) that aerc and other JMAP clients connect to with OAuth.
- Outbound mail through SMTP2GO.
- Expected cost: roughly $2–4 per month for personal volume ([costs](costs.md)).

## Before you start: things that are hard to undo

- **Your domain's mail moves.** Delegating the domain to the new Route 53 zone and publishing the SES MX record sends all mail for that domain to s3mail. Any other DNS records the domain uses (website, verification records) must be recreated in the new zone first ([step 5](#5-delegate-the-domain)).
- **One active SES receipt rule set per region.** s3mail creates and activates its own rule set. If this account already receives mail with SES in that region, the existing rule set is deactivated. Use another region or another account.
- **Originals cannot be deleted.** Received and sent messages are kept in a versioned S3 bucket that denies deletes to every principal. Deleting a message in a client only hides it. See [retention](../operations/recovery.md#original-mail-retention).
- **No backups.** DynamoDB holds mailbox metadata without point-in-time recovery. If it is lost, metadata is lost; the original messages remain.

## Prerequisites

| You need | Notes |
| --- | --- |
| An AWS account | Sign in as a non-root administrator with short-lived credentials (IAM Identity Center or `aws login`). Do not use root or long-lived access keys. |
| A domain you control | You must be able to change its nameservers at the registrar, or add NS records in the parent zone for a subdomain. |
| An [SMTP2GO](https://www.smtp2go.com/) account | The free plan allows 1,000 messages a month and 200 a day. |
| Terraform **1.16.6** exactly | `infra/*/versions.tf` pins it; `mise` or `tfenv` can install it. |
| Go 1.27 or later | `go/go.mod` sets the toolchain (`go1.27.2`). |
| AWS CLI v2, `make`, `zip`, `git`, `jq`, `dig` | |
| macOS for `mail-auth` | The sign-in helper stores tokens in the macOS Keychain only; Linux support is planned. |
| [aerc](https://aerc-mail.org/) | The tested mail client ([usage](usage.md)). |

Throughout this guide, replace `example.com` with your domain and `us-east-1` with your region. Commands run from the repository root.

## 1. Choose a region

SES receives mail only in some regions. Pick one from the [SES email receiving endpoints](https://docs.aws.amazon.com/general/latest/gr/ses.html); every s3mail resource uses that region. The reference deployment uses `us-east-1`.

Check that the region has no active receipt rule set:

```sh
aws ses describe-active-receipt-rule-set --region us-east-1
```

An empty response means you can continue. If a rule set is returned, choose another region or account (see [above](#before-you-start-things-that-are-hard-to-undo)).

## 2. Build the Lambda functions and helpers

```sh
git clone https://github.com/zero11-0203/s3mail.git
cd s3mail
make build                                    # Lambda zips in go/build/<function>/bootstrap.zip
(cd go && go install ./cmd/mail-auth ./cmd/mail-admin)
```

`go install` puts `mail-auth` and `mail-admin` in `$(go env GOPATH)/bin`; make sure that directory is on your `PATH`.

## 3. Bootstrap the Terraform state bucket

`infra/bootstrap` creates the versioned state bucket, an artifact bucket and the GitHub OIDC provider. It is applied once, by hand. Because the state bucket does not exist yet, the first apply uses local state, which is then moved into the bucket.

```sh
cp infra/bootstrap/terraform.tfvars.example infra/bootstrap/terraform.tfvars
# edit: region = "us-east-1"

mv infra/bootstrap/backend.tf infra/bootstrap/backend.tf.off
terraform -chdir=infra/bootstrap init
terraform -chdir=infra/bootstrap apply

cat > infra/backend.hcl <<EOF
bucket = "$(terraform -chdir=infra/bootstrap output -raw state_bucket)"
region = "us-east-1"
EOF

mv infra/bootstrap/backend.tf.off infra/bootstrap/backend.tf
terraform -chdir=infra/bootstrap init -backend-config=../backend.hcl -migrate-state
```

Answer `yes` to copy the state. After `terraform -chdir=infra/bootstrap plan` shows no changes, delete the leftover local `infra/bootstrap/terraform.tfstate*` files.

If the account already has a GitHub OIDC provider (`token.actions.githubusercontent.com`), the apply fails with `EntityAlreadyExists`. Import the existing one first:

```sh
terraform -chdir=infra/bootstrap import aws_iam_openid_connect_provider.github \
  arn:aws:iam::<account-id>:oidc-provider/token.actions.githubusercontent.com
```

`infra/backend.hcl` and every `terraform.tfvars` are gitignored because they name your account's resources. Keep them out of commits.

## 4. Configure the mail system

```sh
cp infra/main/terraform.tfvars.example infra/main/terraform.tfvars
terraform -chdir=infra/main init -backend-config=../backend.hcl
```

Edit `infra/main/terraform.tfvars`:

| Variable | Value |
| --- | --- |
| `region` | The region from step 1 |
| `domain` | Your domain, or the subdomain you deploy on |
| `mail_aliases` | The local parts that receive mail, for example `["alice", "shop"]`. Mail to any other address is rejected during the SMTP conversation. `canary` is added automatically. |
| `auth_domain_prefix` | A globally unique prefix for the Cognito sign-in page, for example `s3mail-7f3k2q` |
| `owner_subject` | Keep the placeholder from the example for now; [step 7](#7-create-your-user) replaces it |
| `smtp2go_cnames` | Leave unset for now; [step 8](#8-set-up-sending) fills it |
| `relay_quota` | Defaults to SMTP2GO's free plan (200 a day, 1,000 a month). Change it if your plan differs. |

Other variables (log retention, webmail) are described in `infra/main/variables.tf`.

## 5. Delegate the domain

First create the hosted zone and the records it depends on:

```sh
terraform -chdir=infra/main apply -target=module.dns
terraform -chdir=infra/main output dns_name_servers
```

Before you change nameservers, recreate in the new zone any records the domain still needs (website `A`/`CNAME`, verification `TXT` and so on). Add them in the Route 53 console; Terraform leaves records it does not declare alone.

Then point the domain at the four nameservers:

- **Whole domain:** set them as the domain's nameservers at your registrar.
- **Subdomain** (for example `mail.example.com`): add an `NS` record for the subdomain with these four values in the parent domain's DNS.

Wait until public resolvers return them:

```sh
dig +short NS example.com @1.1.1.1
```

## 6. Apply

Apply everything:

```sh
terraform -chdir=infra/main plan -out=main.tfplan
terraform -chdir=infra/main apply main.tfplan
```

The apply waits until SES has verified the domain through the `_amazonses` TXT record; with working delegation this takes seconds. If it times out, check delegation and run the plan and apply again.

## 7. Create your user

Create the Cognito user. The temporary password is read without echo, so it does not reach your shell history; you choose the real password at first sign-in.

```sh
POOL_ID=$(terraform -chdir=infra/main output -json auth | jq -r '.issuer | split("/") | last')
read -rs TEMP_PW
aws cognito-idp admin-create-user --user-pool-id "$POOL_ID" --username alice \
  --temporary-password "$TEMP_PW" --message-action SUPPRESS
unset TEMP_PW
aws cognito-idp admin-get-user --user-pool-id "$POOL_ID" --username alice \
  --query "UserAttributes[?Name=='sub'].Value" --output text
```

Put the printed `sub` into `owner_subject` in `infra/main/terraform.tfvars` and apply again (plan, then apply the saved plan as above). Only this subject can open the mailbox.

Sign-in uses a password only. The minimum length is 8 characters and MFA is off, so use a long, unique, generated password.

## 8. Set up sending

s3mail starts with sending **paused**. To send through SMTP2GO:

1. In SMTP2GO, add your domain as a verified sender domain. The dashboard shows three CNAME records (DKIM, return path and link domain).
2. Add them to `infra/main/terraform.tfvars`, with names relative to your domain:

   ```hcl
   smtp2go_cnames = [
     { name = "s123456._domainkey", target = "dkim.smtp2go.net",   purpose = "DKIM" },
     { name = "em123456",           target = "return.smtp2go.net", purpose = "return path" },
     { name = "link",               target = "track.smtp2go.net",  purpose = "link domain" },
   ]
   ```

   Copy the exact names and targets from your dashboard; the values above only show the format. Plan and apply, then click **Verify** in SMTP2GO.
3. In SMTP2GO, turn off open and click tracking, and create an SMTP user.
4. Store the SMTP credentials as `{"username": ..., "password": ...}` JSON in the relay secret. The file keeps the password out of your shell history:

   ```sh
   RELAY_SECRET=$(terraform -chdir=infra/main output -json mail_outbound | jq -r .relay_secret_arn)
   (umask 077; ${EDITOR:-vi} relay.json)     # {"username":"...","password":"..."}
   aws secretsmanager put-secret-value --secret-id "$RELAY_SECRET" --secret-string file://relay.json
   rm relay.json
   ```

5. Create the webhook secret that SMTP2GO uses to report deliveries and bounces:

   ```sh
   WEBHOOK_SECRET=$(terraform -chdir=infra/main output -json mail_outbound | jq -r .webhook_secret)
   WEBHOOK_FILE=$(mktemp)                                # private temporary file outside the checkout
   openssl rand -hex 32 | tr -d '\n' > "$WEBHOOK_FILE"   # no trailing newline: the receiver compares the secret exactly
   aws secretsmanager put-secret-value --secret-id "$WEBHOOK_SECRET" --secret-string "file://$WEBHOOK_FILE"
   terraform -chdir=infra/main output -json mail_outbound | jq -r .webhook_url
   ```

   In SMTP2GO's webhook settings, add that URL with the header `Authorization: Bearer <secret>`, using the output of `cat "$WEBHOOK_FILE"`, and the events delivered, bounce, spam and reject. Then delete the file with `rm "$WEBHOOK_FILE"`.
6. Enable sending:

   ```sh
   PARAM=$(terraform -chdir=infra/main output -json mail_outbound | jq -r .relay_parameter)
   aws ssm put-parameter --name "$PARAM" --value smtp2go --overwrite
   ```

   Set the value back to `paused` at any time to stop sending without a deployment. Queued submissions wait and continue once sending is enabled again.

## 9. Connect a client

Follow the [usage guide](usage.md#sign-in) to sign in with `mail-auth` and set up aerc.

## 10. Check the deployment

| Check | How |
| --- | --- |
| MX points at SES | `dig +short MX example.com` returns `inbound-smtp.<region>.amazonaws.com` |
| Domain verified | `aws ses get-identity-verification-attributes --identities example.com` shows `Success` |
| Unknown address rejected | Mail to an undeclared address bounces with `550 5.1.1` |
| Receiving | Mail from an outside account to an alias appears in aerc's Inbox. Mail that fails DMARC goes to the **Quarantine** mailbox. |
| Sending | Mail from aerc to an outside account arrives, and its headers show DKIM, SPF and DMARC `pass` |
| Delivery status | `mail-admin -table <table> submissions` lists the submission; the table name is in `terraform -chdir=infra/main output -json mail_inbound` |
| Dashboard | `terraform -chdir=infra/main output dashboard_url` shows no messages in the DLQs |

There are no alarms by default ([monitoring](../operations/monitoring.md#alarms-and-dashboard)). Look at the dashboard after you change something. Consider adding an [AWS Budgets](https://docs.aws.amazon.com/cost-management/latest/userguide/budgets-create.html) alert sent to an address outside this mail system.

## Updating

```sh
git pull
make build
terraform -chdir=infra/main plan -out=main.tfplan
terraform -chdir=infra/main apply main.tfplan
```

Read the plan before you apply it. Stop if it destroys or replaces the hosted zone, the MX record, a bucket, the DynamoDB table, the Cognito user pool or the SES identity or rules. Code-only releases change Lambda versions and `live` aliases only. Delete `main.tfplan` afterwards; saved plans can contain sensitive values.

## Optional: browser webmail

The browser webmail deploys from its own Terraform root and needs Docker to build its image; see [webmail deployment](../operations/deployment.md#webmail-deployment).

## Optional: mail agent and semantic search

Both are off by default and send mail text to an external model API (OpenRouter), so read [data leaving AWS](../specs/security.md#data-leaving-aws) before you enable them.

- **Semantic search** adds embeddings to the search index; see [semantic search](../operations/deployment.md#semantic-search).
- **The mail agent** answers questions about your mail from the command line and can triage new mail. It deploys from its own Terraform root (`infra/agent`) on Amazon Bedrock AgentCore, needs Docker and [uv](https://docs.astral.sh/uv/), and accesses mail only through JMAP; see the [agent spec](../specs/agent.md) and [agent deployment](../operations/deployment.md#agent-deployment).

## Removing a deployment

Removal is deliberately manual, because the data protections block it:

- The hosted zone, buckets, table and user pool have `prevent_destroy`.
- The table and user pool have deletion protection.
- The original-mail bucket policy denies deletes to every principal.

Leaving the stopped stack in place costs mostly the hosted zone, the secrets and S3 storage ([costs](costs.md)). Before you remove anything, move the domain's MX record elsewhere so mail is not lost.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `terraform init` fails with an unsupported Terraform version | Install exactly 1.16.6 |
| SES verification waits or times out | Nameservers not delegated yet, or not yet visible to public resolvers |
| A sender gets `550 5.1.1 mailbox unavailable` | The address is not in `mail_aliases`; plus-tags (`alice+shop@`) of declared aliases are accepted |
| aerc gets `403` on every request | `owner_subject` does not match your user's `sub` |
| Sending stays queued | The relay parameter is `paused`, the relay secret is not set, or the quota is used up |
| A submission is `acceptance unknown` | The relay connection broke after the message was handed over. Check SMTP2GO's activity log, then run `mail-admin -table <table> resolve <id> accepted\|rejected\|retry` ([usage](usage.md#sending-limits-and-failed-sends)). s3mail never resends this automatically. |
