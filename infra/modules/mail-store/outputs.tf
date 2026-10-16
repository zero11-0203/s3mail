output "table_name" {
  value = aws_dynamodb_table.mail.name
}

output "table_arn" {
  value = aws_dynamodb_table.mail.arn
}

output "account_id" {
  description = "Internal account id that owns the partition."
  value       = var.account_id
}

output "account_partition" {
  description = "Partition key value, for dynamodb:LeadingKeys conditions."
  value       = "ACCOUNT#${var.account_id}"
}
