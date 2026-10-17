output "submit_queue_arn" {
  value = aws_sqs_queue.submit.arn
}

output "submit_dlq_arn" {
  value = aws_sqs_queue.submit_dlq.arn
}

output "relay_secret_arn" {
  description = "Set its value out of band: {\"username\": ..., \"password\": ...}."
  value       = aws_secretsmanager_secret.relay.arn
}

output "relay_parameter" {
  description = "Set to the relay name to send, or paused."
  value       = aws_ssm_parameter.relay.name
}

output "function_names" {
  value = { for k, f in aws_lambda_function.fn : k => f.function_name }
}

output "webhook" {
  description = "The webhook receiver for the API's /webhooks route."
  value = {
    function_name = aws_lambda_function.fn["webhook"].function_name
    alias_name    = aws_lambda_alias.live["webhook"].name
    alias_arn     = aws_lambda_alias.live["webhook"].arn
    relay         = var.relay.name
  }
}

output "webhook_secret_arn" {
  description = "Set its value out of band; SMTP2GO sends it as Authorization: Bearer <value>."
  value       = aws_secretsmanager_secret.webhook.arn
}

output "delivery_dlq_arn" {
  value = aws_sqs_queue.delivery_dlq.arn
}

output "delivery_queue_arn" {
  value = aws_sqs_queue.delivery.arn
}
