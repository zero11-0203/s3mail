output "dns_records" {
  description = "Records for the dns module: SES MX, SES ownership verification and DMARC."
  value = [
    {
      name    = ""
      type    = "MX"
      ttl     = var.dns_ttl
      values  = ["10 inbound-smtp.${local.region}.amazonaws.com"]
      purpose = "SES inbound receiving endpoint"
    },
    {
      name    = "_amazonses"
      type    = "TXT"
      ttl     = var.dns_ttl
      values  = [aws_ses_domain_identity.this.verification_token]
      purpose = "SES receiving domain ownership verification"
    },
    {
      name    = "_dmarc"
      type    = "TXT"
      ttl     = var.dns_ttl
      values  = [var.dmarc_policy]
      purpose = "DMARC policy"
    },
  ]
}

output "domain_identity" {
  description = "SES domain identity, for the verification wait in the root."
  value       = aws_ses_domain_identity.this.domain
}

output "originals_bucket" {
  description = "Permanent original-mail bucket name."
  value       = aws_s3_bucket.originals.id
}

output "originals_bucket_arn" {
  value = aws_s3_bucket.originals.arn
}

output "ses_key_prefix" {
  value = var.ses_key_prefix
}

output "receipts_topic_arn" {
  value = aws_sns_topic.receipts.arn
}

output "ingest_queue_arn" {
  value = aws_sqs_queue.ingest.arn
}

output "ingest_queue_url" {
  value = aws_sqs_queue.ingest.id
}

output "ingest_dlq_arn" {
  value = aws_sqs_queue.ingest_dlq.arn
}

output "delivery_dlq_arn" {
  value = aws_sqs_queue.delivery_dlq.arn
}

output "recipients" {
  description = "Addresses the active receipt rule accepts."
  value       = local.recipients
}
