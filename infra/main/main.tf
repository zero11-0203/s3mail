# Root configuration for the single deployed environment. Modules (dns,
# mail-inbound, mail-outbound, relay-smtp2go, jmap-api, search, auth, ops)
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
  records   = module.mail_inbound.dns_records
}

module "mail_inbound" {
  source = "../modules/mail-inbound"

  domain  = var.domain
  aliases = var.mail_aliases
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
