variable "region" {
  description = "AWS region; the same one as infra/main, with AgentCore available."
  type        = string
}

variable "project" {
  description = "Project name used for resource names and tags."
  type        = string
  default     = "s3mail"
}

variable "jmap_session_url" {
  description = "infra/main output agent.jmap_session_url."
  type        = string
}

variable "oauth_issuer" {
  description = "infra/main output agent.oauth_issuer."
  type        = string
}

variable "oauth_client_id" {
  description = "infra/main output agent.oauth_client_id."
  type        = string
}

variable "oauth_scopes" {
  description = "infra/main output agent.oauth_scopes."
  type        = list(string)
}

variable "ask_model" {
  description = "Pinned OpenRouter model id for questions."
  type        = string
  default     = "openai/gpt-6.1-sol"
}

variable "image_digest" {
  description = "Digest of the pushed agent image (sha256:...), printed by make agent-image. Null deploys no runtime."
  type        = string
  default     = null

  validation {
    condition     = var.image_digest == null || can(regex("^sha256:[0-9a-f]{64}$", coalesce(var.image_digest, "-")))
    error_message = "image_digest is sha256:<64 hex>."
  }
}

variable "live_version" {
  description = "Runtime version behind live; null follows the latest. Set to roll back."
  type        = string
  default     = null
}

variable "retained_images" {
  description = "Images kept in the repository for rollback."
  type        = number
  default     = 10
}

variable "budget" {
  description = "Monthly agent budget alert (set in terraform.tfvars); null creates none."
  type = object({
    monthly_usd = number
    email       = string
  })
  default = null
}
