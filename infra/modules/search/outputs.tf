output "search_function_arn" {
  description = "The search function's live alias; the JMAP API invokes only this."
  value       = aws_lambda_alias.live["search"].arn
}

output "index_bucket" {
  value = aws_s3_bucket.index.id
}

output "index_queue_arn" {
  value = aws_sqs_queue.index.arn
}

output "index_dlq_arn" {
  value = aws_sqs_queue.index_dlq.arn
}

output "index_queue_url" {
  value = aws_sqs_queue.index.url
}

output "function_names" {
  value = { for k, f in aws_lambda_function.fn : k => f.function_name }
}
