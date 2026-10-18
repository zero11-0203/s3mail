output "user_pool_id" {
  value = aws_cognito_user_pool.this.id
}

output "issuer" {
  description = "Exact iss value of access tokens; the JWKS is at <issuer>/.well-known/jwks.json."
  value       = "https://cognito-idp.${data.aws_region.current.region}.amazonaws.com/${aws_cognito_user_pool.this.id}"
}

output "terminal_client_id" {
  value = aws_cognito_user_pool_client.terminal.id
}

output "webmail_client_id" {
  description = "The Phase 4 webmail app client, or null while none is registered."
  value       = one(aws_cognito_user_pool_client.webmail[*].id)
}

output "agent_client_id" {
  description = "The mail agent's app client, or null while none is registered."
  value       = one(aws_cognito_user_pool_client.agent[*].id)
}

output "agent_scopes" {
  description = "Exact wire scopes granted to the agent client."
  value       = [for s in var.agent_scopes : "${aws_cognito_resource_server.s3mail.identifier}/${s}"]
}

output "login_base_url" {
  description = "Base URL for /oauth2/authorize and /oauth2/token."
  value       = "https://${aws_cognito_user_pool_domain.this.domain}.auth.${data.aws_region.current.region}.amazoncognito.com"
}

output "scopes" {
  description = "Exact wire scopes (docs/specs/security.md#scopes)."
  value       = [for s in aws_cognito_resource_server.s3mail.scope : "${aws_cognito_resource_server.s3mail.identifier}/${s.scope_name}"]
}
