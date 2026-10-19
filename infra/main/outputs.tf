output "dns_name_servers" {
  description = "Route 53 nameservers to set at the registrar."
  value       = module.dns.name_servers
}

output "mail_inbound" {
  description = "Receiving resources the ingestion Lambda and checks need."
  value = {
    recipients       = module.mail_inbound.recipients
    originals_bucket = module.mail_inbound.originals_bucket
    ingest_queue_url = module.mail_inbound.ingest_queue_url
    ingest_function  = module.mail_inbound.ingest_function
    table_name       = module.mail_store.table_name
  }
}

output "auth" {
  description = "Values mail-auth and the authorizer need."
  value = {
    issuer             = module.auth.issuer
    terminal_client_id = module.auth.terminal_client_id
    login_base_url     = module.auth.login_base_url
    scopes             = module.auth.scopes
    callback_url       = var.terminal_callback_urls[0]
  }
}

output "webmail" {
  description = "Inputs for infra/webmail (its terraform.tfvars), or null while no webmail is registered."
  value = local.webmail_origin == null ? null : {
    domain          = var.domain
    subdomain       = var.webmail_subdomain
    jmap_server_url = module.jmap_api.base_url
    oauth_issuer    = module.auth.issuer
    oauth_client_id = module.auth.webmail_client_id
    oauth_scopes    = module.auth.scopes
  }
}

output "agent" {
  description = "Inputs for infra/agent (its terraform.tfvars), or null while no agent client is registered."
  value = module.auth.agent_client_id == null ? null : {
    jmap_session_url = "${module.jmap_api.base_url}/.well-known/jmap"
    oauth_issuer     = module.auth.issuer
    oauth_client_id  = module.auth.agent_client_id
    oauth_scopes     = module.auth.agent_scopes
    user_pool_id     = module.auth.user_pool_id
    event_bus_name   = module.mail_outbound.event_bus.name
  }
}

output "jmap" {
  description = "JMAP endpoints for aerc and probes."
  value = {
    session_url           = "${module.jmap_api.base_url}/.well-known/jmap"
    resource_metadata_url = module.jmap_api.resource_metadata_url
    function_names        = module.jmap_api.function_names
  }
}

output "mail_outbound" {
  description = "Sending configuration set out of band and the submission queues."
  value = {
    relay_secret_arn = module.mail_outbound.relay_secret_arn
    relay_parameter  = module.mail_outbound.relay_parameter
    webhook_url      = module.jmap_api.webhook_url
    webhook_secret   = module.mail_outbound.webhook_secret_arn
    delivery_dlq_arn = module.mail_outbound.delivery_dlq_arn
    submit_dlq_arn   = module.mail_outbound.submit_dlq_arn
    function_names   = module.mail_outbound.function_names
  }
}

output "dashboard_url" {
  description = "The CloudWatch operations dashboard."
  value       = module.ops.dashboard_url
}
