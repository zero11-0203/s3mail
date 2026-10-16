# Partial configuration: bucket and region come from infra/backend.hcl
# (gitignored), so the account-specific bucket name stays out of the repo.
terraform {
  backend "s3" {
    key          = "main/terraform.tfstate"
    use_lockfile = true
    encrypt      = true
  }
}
