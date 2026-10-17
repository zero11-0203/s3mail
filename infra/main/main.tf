# Root configuration for the single deployed environment. Modules (dns,
# mail-store, mail-inbound, mail-outbound, relay-smtp2go, jmap-api, search, auth, ops)
# are added as each phase needs them; see docs/operations/deployment.md.

# Phase 0 streaming spike; temporary and disabled by default.
module "spike_stream" {
  source = "../modules/spike-stream"
  count  = var.spike_stream_enabled ? 1 : 0

  lambda_zip     = var.spike_stream_zip
  authorizer_zip = var.spike_authorizer_zip
  issuer         = module.auth.issuer
  client_ids     = [module.auth.terminal_client_id]
  scopes         = module.auth.scopes
}

# The hosted zone for the whole domain. Modules add dns_records outputs to
# this list as they are introduced (mail-inbound, relay-*, jmap-api).
module "dns" {
  source = "../modules/dns"

  zone_name = var.domain
  records   = concat(module.mail_inbound.dns_records, module.relay_smtp2go.dns_records)
}

module "mail_store" {
  source = "../modules/mail-store"
}

module "mail_inbound" {
  source = "../modules/mail-inbound"

  domain            = var.domain
  aliases           = var.mail_aliases
  ingest_zip        = var.ingest_zip
  table_name        = module.mail_store.table_name
  table_arn         = module.mail_store.table_arn
  account_id        = module.mail_store.account_id
  account_partition = module.mail_store.account_partition
}

# Waits until SES sees the _amazonses TXT record the dns module published.
resource "aws_ses_domain_identity_verification" "inbound" {
  domain = module.mail_inbound.domain_identity

  depends_on = [module.dns]
}

module "auth" {
  source = "../modules/auth"

  domain_prefix          = var.auth_domain_prefix
  terminal_callback_urls = var.terminal_callback_urls
}

module "jmap_api" {
  source = "../modules/jmap-api"

  api_zip               = var.jmap_api_zip
  download_zip          = var.jmap_download_zip
  push_zip              = var.jmap_push_zip
  authorizer_zip        = var.authorizer_zip
  issuer                = module.auth.issuer
  client_ids            = [module.auth.terminal_client_id]
  scopes                = module.auth.scopes
  owner_subject         = var.owner_subject
  table_name            = module.mail_store.table_name
  table_arn             = module.mail_store.table_arn
  account_id            = module.mail_store.account_id
  account_partition     = module.mail_store.account_partition
  originals_bucket_name = module.mail_inbound.originals_bucket
  originals_bucket_arn  = module.mail_inbound.originals_bucket_arn
  ses_key_prefix        = module.mail_inbound.ses_key_prefix
  search_function_arn   = module.search.search_function_arn
  webhook               = module.mail_outbound.webhook
  sending = {
    domain        = var.domain
    aliases       = [for a in var.mail_aliases : a if a != "canary"]
    relay         = module.relay_smtp2go.relay.name
    daily         = var.relay_quota.daily
    monthly       = var.relay_quota.monthly
    per_recipient = var.relay_quota.per_recipient
    timezone      = var.relay_quota.timezone
  }
}

module "search" {
  source = "../modules/search"

  indexer_zip          = var.search_indexer_zip
  search_zip           = var.search_zip
  table_name           = module.mail_store.table_name
  table_arn            = module.mail_store.table_arn
  account_id           = module.mail_store.account_id
  account_partition    = module.mail_store.account_partition
  originals_bucket_arn = module.mail_inbound.originals_bucket_arn
}

module "relay_smtp2go" {
  source = "../modules/relay-smtp2go"

  cnames = var.smtp2go_cnames
}

module "mail_outbound" {
  source = "../modules/mail-outbound"

  dispatcher_zip       = var.outbox_dispatcher_zip
  worker_zip           = var.submission_worker_zip
  recovery_zip         = var.submission_recovery_zip
  webhook_zip          = var.relay_webhook_zip
  delivery_zip         = var.delivery_worker_zip
  domain               = var.domain
  relay                = module.relay_smtp2go.relay
  table_name           = module.mail_store.table_name
  table_arn            = module.mail_store.table_arn
  stream_arn           = module.mail_store.stream_arn
  account_id           = module.mail_store.account_id
  account_partition    = module.mail_store.account_partition
  originals_bucket_arn = module.mail_inbound.originals_bucket_arn
  index_queue_url      = module.search.index_queue_url
  index_queue_arn      = module.search.index_queue_arn
}
