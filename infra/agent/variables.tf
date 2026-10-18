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

variable "triage_model" {
  description = "Pinned OpenRouter model id for triage and the digest overview."
  type        = string
  default     = "openai/gpt-6-luna"
}

variable "features" {
  description = "Optional A4 agent tools: attachments, semantic."
  type        = list(string)
  default     = []
}

variable "event_bus_name" {
  description = "infra/main output agent.event_bus_name; null creates no A1 trigger. Run make agent-build first."
  type        = string
  default     = null
}

variable "trigger_zip" {
  description = "Trigger Lambda zip, built by make agent-build."
  type        = string
  default     = "../../go/build/agent-trigger/bootstrap.zip"
}

variable "digest_schedule" {
  description = "Daily digest delivered as mail, such as cron(0 7 * * ? *) (UTC); needs mail.write. Null schedules none."
  type        = string
  default     = null
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
  description = "The approved runtime version behind live: \"1\" on the first deployment, then a smoke-tested candidate_version, or an earlier version to roll back."
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
