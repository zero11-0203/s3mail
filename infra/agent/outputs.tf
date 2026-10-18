output "repository_url" {
  description = "Push target for make agent-image (AGENT_REPO)."
  value       = aws_ecr_repository.agent.repository_url
}

output "callback_url" {
  description = "Set as agent_callback_url in infra/main."
  value       = module.agent.callback_url
}

output "client_secret_arn" {
  description = "Secret the owner fills with the agent client's secret."
  value       = module.agent.client_secret_arn
}

output "model_key_secret_arn" {
  description = "Secret the owner fills with {\"api_key\": ...}, the OpenRouter key."
  value       = module.agent.model_key_secret_arn
}

output "runtime_arn" {
  description = "S3MAIL_AGENT_RUNTIME_ARN for the agent CLI, once an image is deployed."
  value       = module.agent.runtime_arn
}

output "runtime_version" {
  description = "Version behind live, for rollback."
  value       = module.agent.runtime_version
}

output "candidate_version" {
  description = "Newest runtime version, on the DEFAULT endpoint: smoke-test it with agent -qualifier DEFAULT, then set live_version to it."
  value       = module.agent.candidate_version
}

output "triage_dlq_url" {
  description = "Failed triage events: inspect, fix the cause (often agent login), then redrive."
  value       = module.agent.triage_dlq_url
}

output "workload_identity_arn" {
  description = "The runtime's workload identity; allow the login return URL on it once."
  value       = module.agent.workload_identity_arn
}
