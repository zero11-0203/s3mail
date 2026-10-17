variable "region" {
  description = "AWS region; the same one as infra/main."
  type        = string
}

variable "project" {
  description = "Project name used for resource names and tags."
  type        = string
  default     = "s3mail"
}

variable "domain" {
  description = "Mail domain whose Route 53 zone infra/main owns (infra/main output webmail.domain)."
  type        = string
}

variable "subdomain" {
  description = "Webmail host under domain (infra/main output webmail.subdomain)."
  type        = string
}

variable "jmap_server_url" {
  description = "JMAP API base URL (infra/main output webmail.jmap_server_url)."
  type        = string
}

variable "oauth_issuer" {
  description = "Cognito issuer (infra/main output webmail.oauth_issuer)."
  type        = string
}

variable "oauth_client_id" {
  description = "Webmail app client (infra/main output webmail.oauth_client_id)."
  type        = string
}

variable "oauth_scopes" {
  description = "Wire scopes (infra/main output webmail.oauth_scopes)."
  type        = list(string)
}

variable "image_digest" {
  description = "Digest of the pushed webmail image (sha256:...), printed by make webmail-image. Null deploys only the repository."
  type        = string
  default     = null

  validation {
    condition     = var.image_digest == null || can(regex("^sha256:[0-9a-f]{64}$", var.image_digest))
    error_message = "image_digest is sha256:<64 hex>."
  }
}

variable "concurrency" {
  description = "Reserved concurrency of the webmail function."
  type        = number
  default     = 4
}

variable "retained_images" {
  description = "Images kept in the repository for rollback."
  type        = number
  default     = 10
}

variable "budget" {
  description = "Monthly webmail budget alert (set in terraform.tfvars); null creates none."
  type = object({
    monthly_usd = number
    email       = string
  })
  default = null
}
