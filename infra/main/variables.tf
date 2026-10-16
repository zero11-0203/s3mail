variable "region" {
  description = "AWS region for every project resource; must be an SES email receiving region (docs/operations/deployment.md#dns-and-region)."
  type        = string
}

variable "project" {
  description = "Project name used for resource names and tags."
  type        = string
  default     = "s3mail"
}

variable "domain" {
  description = "Mail domain served by the Route 53 hosted zone (set in terraform.tfvars)."
  type        = string
}

variable "mail_aliases" {
  description = "Local parts that receive mail at the domain (set in terraform.tfvars); canary is added automatically."
  type        = list(string)
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

variable "spike_authorizer_zip" {
  description = "Path to the authorizer Lambda zip built by make spike-build."
  type        = string
  default     = "../../go/build/authorizer/bootstrap.zip"
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
