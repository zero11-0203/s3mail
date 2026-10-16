output "state_bucket" {
  description = "Terraform state bucket; put it in infra/backend.hcl."
  value       = aws_s3_bucket.this["state"].id
}

output "artifact_bucket" {
  description = "Versioned bucket for Lambda zips keyed by commit SHA."
  value       = aws_s3_bucket.this["artifact"].id
}

output "github_oidc_provider_arn" {
  description = "IAM OIDC provider trusted by the CI roles."
  value       = aws_iam_openid_connect_provider.github.arn
}
