variable "name" {
  description = "Runtime name; AgentCore names allow letters, digits and underscores only."
  type        = string
  default     = "s3mail_agent"

  validation {
    condition     = can(regex("^[a-zA-Z][a-zA-Z0-9_]{0,40}$", var.name))
    error_message = "name starts with a letter and has only letters, digits and underscores."
  }
}

variable "resource_prefix" {
  description = "Prefix of the role, key alias, secret and credential provider."
  type        = string
  default     = "s3mail-agent"
}

variable "image_uri" {
  description = "The agent image by immutable digest (<repository>@sha256:...), built by make agent-image. Null deploys no role, runtime or endpoint."
  type        = string
  default     = null

  validation {
    condition     = var.image_uri == null || can(regex("^[^:@]+(:[0-9]+)?/[^:@]+@sha256:[0-9a-f]{64}$", coalesce(var.image_uri, "-")))
    error_message = "Deploy by digest (<repository>@sha256:<64 hex>), never by a tag."
  }
}

variable "repository_arn" {
  description = "The agent ECR repository; the only one the runtime may pull from."
  type        = string
}

variable "jmap_session_url" {
  description = "JMAP session URL (infra/main output agent.jmap_session_url)."
  type        = string
}

variable "oauth_issuer" {
  description = "Cognito issuer (infra/main output agent.oauth_issuer); discovery is <issuer>/.well-known/openid-configuration."
  type        = string
}

variable "oauth_client_id" {
  description = "The agent's confidential Cognito app client (infra/main output agent.oauth_client_id)."
  type        = string
}

variable "oauth_scopes" {
  description = "Wire scopes the agent requests (infra/main output agent.oauth_scopes)."
  type        = list(string)

  validation {
    condition     = length(var.oauth_scopes) > 0
    error_message = "oauth_scopes is not empty."
  }
}

variable "oauth_return_url" {
  description = "Where the token vault returns after consent: agent login's one-shot localhost callback. It must also be allowed on the runtime's workload identity."
  type        = string
  default     = "http://localhost:8765/callback"
}

variable "ask_model" {
  description = "Pinned OpenRouter model id for questions; a change is a release with an evaluation run."
  type        = string
  default     = "openai/gpt-6.1-sol"
}

variable "model_base_url" {
  description = "OpenAI-compatible model API base URL."
  type        = string
  default     = "https://openrouter.ai/api/v1"

  validation {
    condition     = startswith(var.model_base_url, "https://")
    error_message = "model_base_url uses HTTPS."
  }
}

variable "memory_expiry_days" {
  description = "Checkpoint event expiry (docs/specs/agent.md#state-and-memory)."
  type        = number
  default     = 7
}

variable "idle_session_seconds" {
  type    = number
  default = 900
}

variable "max_session_seconds" {
  type    = number
  default = 3600
}

variable "live_version" {
  description = "The approved runtime version behind live: \"1\" on the first deployment, then the smoke-tested candidate to promote, or an earlier version to roll back."
  type        = string
  default     = null

  validation {
    condition     = var.image_uri == null || can(regex("^[0-9]+$", coalesce(var.live_version, "-")))
    error_message = "With an image, set live_version to the approved runtime version; live never follows a new version by itself."
  }
}

variable "log_retention_days" {
  description = "Retention of the runtime's log groups (docs/operations/monitoring.md)."
  type        = number
  default     = 30
}
