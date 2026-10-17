# Partial configuration shared with infra/main: bucket and region come from
# infra/backend.hcl (gitignored). A separate key keeps this state apart.
# Initialize with: terraform init -backend-config=../backend.hcl
terraform {
  backend "s3" {
    key          = "webmail/terraform.tfstate"
    use_lockfile = true
    encrypt      = true
  }
}
