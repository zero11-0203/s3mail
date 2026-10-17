variable "name" {
  description = "Name of the function, role, log group and API."
  type        = string
  default     = "s3mail-webmail"
}

variable "image_uri" {
  description = "The webmail image by immutable digest (<repository>@sha256:...), built by make webmail-image."
  type        = string

  validation {
    condition     = can(regex("^[^:@]+(:[0-9]+)?/[^:@]+@sha256:[0-9a-f]{64}$", var.image_uri))
    error_message = "Deploy by digest (<repository>@sha256:<64 hex>), never by a tag."
  }
}

variable "domain" {
  description = "The mail domain whose hosted zone serves the webmail host."
  type        = string
}

variable "subdomain" {
  description = "The webmail host under domain; must match webmail_subdomain in infra/main."
  type        = string
}

variable "zone_id" {
  description = "Route 53 hosted zone of domain, owned by infra/main."
  type        = string
}

variable "jmap_server_url" {
  description = "JMAP API base URL (infra/main output webmail.jmap_server_url)."
  type        = string
}

variable "oauth_issuer" {
  description = "Cognito issuer URL; the server discovers its endpoints from it."
  type        = string
}

variable "oauth_client_id" {
  description = "The webmail Cognito app client registered by infra/main."
  type        = string
}

variable "oauth_scopes" {
  description = "Exact wire scopes requested at login."
  type        = list(string)
}

variable "app_name" {
  description = "Name shown in the webmail UI."
  type        = string
  default     = "s3mail"
}

variable "log_level" {
  description = "Upstream LOG_LEVEL. Warn and error lines carry no mail; debug/info must not be left on (docs/operations/monitoring.md)."
  type        = string
  default     = "warn"

  validation {
    condition     = contains(["error", "warn"], var.log_level)
    error_message = "log_level is error or warn."
  }
}

variable "memory" {
  description = "Function memory (MB); also sets its CPU share and cold-start time."
  type        = number
  default     = 1024
}

variable "concurrency" {
  description = "Reserved concurrency: the cap on simultaneous webmail requests and their cost. A first page load fetches about 20 assets at once, and 4 throttled it into 503s."
  type        = number
  default     = 20
}

variable "throttle" {
  description = "Stage-wide request rate (per second) and burst."
  type = object({
    rate  = number
    burst = number
  })
  default = { rate = 20, burst = 50 }
}

variable "log_retention_days" {
  type    = number
  default = 30
}
