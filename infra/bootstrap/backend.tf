# Partial configuration: bucket and region come from infra/backend.hcl
# (gitignored). Initialize with:
#   terraform init -backend-config=../backend.hcl
# A fresh account starts without this file, applies with local state, then
# restores it and runs terraform init -migrate-state (docs/specs/security.md#ci-identity).
terraform {
  backend "s3" {
    key          = "bootstrap/terraform.tfstate"
    use_lockfile = true
    encrypt      = true
  }
}
