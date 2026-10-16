output "spike_stream_invoke_url" {
  description = "Stage URL for spike-probe -url, when the spike is enabled."
  value       = one(module.spike_stream[*].invoke_url)
}

output "dns_name_servers" {
  description = "Route 53 nameservers to set at the registrar."
  value       = module.dns.name_servers
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
