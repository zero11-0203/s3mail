output "base_url" {
  description = "Public API root; the session resource is <base_url>/.well-known/jmap."
  value       = local.base_url
}

output "resource_metadata_url" {
  description = "RFC 9728 metadata URL named in 401 challenges."
  value       = local.metadata_url
}

output "function_names" {
  value = { for k, f in aws_lambda_function.fn : k => f.function_name }
}

output "role_arns" {
  value = merge({ for k, r in aws_iam_role.fn : k => r.arn }, { authorizer = aws_iam_role.authorizer.arn })
}

output "webhook_url" {
  description = "The relay webhook URL, or null."
  value       = var.webhook == null ? null : "${local.base_url}/webhooks/${var.webhook.relay}"
}
