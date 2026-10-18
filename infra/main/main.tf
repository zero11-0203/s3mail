# Root configuration for the single deployed environment: the dns,
# mail-store, mail-inbound, mail-outbound, relay-smtp2go, jmap-api, search,
# maintenance, auth and ops modules; see docs/operations/deployment.md.

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

# Phase 4 webmail registration: its Cognito app client, authorizer
# allowlist entry and CORS origin. The webmail itself is deployed
# separately by infra/webmail and needs nothing else from this root.
locals {
  webmail_origin = var.webmail_subdomain == null ? null : "https://${var.webmail_subdomain}.${var.domain}"
}

module "auth" {
  source = "../modules/auth"

  domain_prefix          = var.auth_domain_prefix
  terminal_callback_urls = var.terminal_callback_urls
  webmail_callback_urls  = local.webmail_origin == null ? [] : [for l in var.webmail_locales : "${local.webmail_origin}/${l}/auth/callback"]
  # Later mail agent: its app client only; infra/agent deploys the agent.
  agent_callback_urls = compact([var.agent_callback_url])
}

module "jmap_api" {
  source = "../modules/jmap-api"

  api_zip        = var.jmap_api_zip
  download_zip   = var.jmap_download_zip
  push_zip       = var.jmap_push_zip
  authorizer_zip = var.authorizer_zip
  issuer         = module.auth.issuer
  client_ids = compact([
    module.auth.terminal_client_id,
    module.auth.webmail_client_id,
    var.agent_allowed ? module.auth.agent_client_id : null,
  ])
  cors_origin           = local.webmail_origin
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

# Daily derived-data collection: history pruning and index version purges.
module "maintenance" {
  source = "../modules/maintenance"

  gc_zip            = var.gc_zip
  table_name        = module.mail_store.table_name
  table_arn         = module.mail_store.table_arn
  account_id        = module.mail_store.account_id
  account_partition = module.mail_store.account_partition
  index_bucket      = module.search.index_bucket
}

# The operations dashboard; no alarms or custom metrics, to avoid their cost.
module "ops" {
  source = "../modules/ops"

  queues = {
    ingest          = { arn = module.mail_inbound.ingest_queue_arn }
    index           = { arn = module.search.index_queue_arn }
    submit          = { arn = module.mail_outbound.submit_queue_arn }
    delivery        = { arn = module.mail_outbound.delivery_queue_arn }
    ingest-dlq      = { arn = module.mail_inbound.ingest_dlq_arn, dead_letter = true }
    receipt-dlq     = { arn = module.mail_inbound.delivery_dlq_arn, dead_letter = true }
    index-dlq       = { arn = module.search.index_dlq_arn, dead_letter = true }
    submit-dlq      = { arn = module.mail_outbound.submit_dlq_arn, dead_letter = true }
    relay-event-dlq = { arn = module.mail_outbound.delivery_dlq_arn, dead_letter = true }
  }
  functions = {
    inbound     = [module.mail_inbound.ingest_function]
    jmap        = concat(values(module.jmap_api.function_names), [module.jmap_api.authorizer_function_name])
    outbound    = values(module.mail_outbound.function_names)
    search      = values(module.search.function_names)
    maintenance = [module.maintenance.function_name]
  }
  api = module.jmap_api.api
}
