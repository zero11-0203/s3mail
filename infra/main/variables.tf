variable "region" {
  description = "AWS region for every project resource; must be an SES email receiving region (docs/operations/deployment.md#dns-and-region)."
  type        = string
}

variable "project" {
  description = "Project name used for resource names and tags."
  type        = string
  default     = "s3mail"
}

variable "spike_stream_enabled" {
  description = "Deploy the Phase 0 response-streaming spike."
  type        = bool
  default     = false
}

variable "spike_stream_zip" {
  description = "Path to the spike Lambda zip built by make spike-build."
  type        = string
  default     = "../../go/build/spike-stream/bootstrap.zip"
}

variable "auth_domain_prefix" {
  description = "Globally unique Cognito managed login domain prefix (set in terraform.tfvars)."
  type        = string
}

variable "terminal_callback_urls" {
  description = "mail-auth loopback redirect URIs."
  type        = list(string)
  default     = ["http://localhost:53789/callback"]
}
