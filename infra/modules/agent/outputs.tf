output "callback_url" {
  description = "Token-vault callback: the agent client's only redirect URI (infra/main agent_callback_url)."
  value       = aws_bedrockagentcore_oauth2_credential_provider.cognito.callback_url
}

output "client_secret_arn" {
  description = "Secret the owner fills with {\"client_secret\": ...}."
  value       = aws_secretsmanager_secret.client.arn
}

output "model_key_secret_arn" {
  description = "Secret the owner fills with {\"api_key\": ...} (the OpenRouter key)."
  value       = aws_secretsmanager_secret.model_key.arn
}

output "memory_id" {
  value = aws_bedrockagentcore_memory.this.id
}

output "runtime_arn" {
  description = "For the agent CLI (S3MAIL_AGENT_RUNTIME_ARN), once an image is deployed."
  value       = one(aws_bedrockagentcore_agent_runtime.this[*].agent_runtime_arn)
}

output "runtime_version" {
  description = "Version behind the live endpoint, for rollback."
  value       = one(aws_bedrockagentcore_agent_runtime_endpoint.live[*].agent_runtime_version)
}

output "workload_identity_arn" {
  description = "The runtime's workload identity; its name takes the allowed OAuth return URL."
  value       = one(flatten([for r in aws_bedrockagentcore_agent_runtime.this : r.workload_identity_details[*].workload_identity_arn]))
}
