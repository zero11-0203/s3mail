output "relay" {
  description = "Configuration for mail-outbound."
  value       = { name = "smtp2go", smtp_host = var.smtp_host, smtp_port = var.smtp_port }
}

output "dns_records" {
  description = "Records for the dns module."
  value       = local.records
}
