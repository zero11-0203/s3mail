output "url" {
  description = "The webmail origin."
  value       = "https://${local.fqdn}"
}

output "function_name" {
  value = aws_lambda_function.this.function_name
}

output "version" {
  description = "The function version behind the live alias, for rollback."
  value       = aws_lambda_alias.live.function_version
}
