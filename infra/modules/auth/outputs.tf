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

output "login_base_url" {
  description = "Base URL for /oauth2/authorize and /oauth2/token."
  value       = "https://${aws_cognito_user_pool_domain.this.domain}.auth.${data.aws_region.current.region}.amazoncognito.com"
}

output "scopes" {
  description = "Exact wire scopes (docs/specs/security.md#scopes)."
  value       = [for s in aws_cognito_resource_server.s3mail.scope : "${aws_cognito_resource_server.s3mail.identifier}/${s.scope_name}"]
}
