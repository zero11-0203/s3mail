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

variable "ingest_zip" {
  description = "Path to the ingest Lambda zip built by make build."
  type        = string
  default     = "../../go/build/ingest/bootstrap.zip"
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

variable "owner_subject" {
  description = "The owner's Cognito sub, mapped to the mail account (set in terraform.tfvars)."
  type        = string
}

variable "authorizer_zip" {
  description = "Path to the bearer-token authorizer Lambda zip built by make build."
  type        = string
  default     = "../../go/build/authorizer/bootstrap.zip"
}

variable "jmap_api_zip" {
  description = "Path to the jmap-api Lambda zip built by make build."
  type        = string
  default     = "../../go/build/jmap-api/bootstrap.zip"
}

variable "jmap_download_zip" {
  description = "Path to the jmap-download Lambda zip built by make build."
  type        = string
  default     = "../../go/build/jmap-download/bootstrap.zip"
}

variable "jmap_push_zip" {
  description = "Path to the jmap-push Lambda zip built by make build."
  type        = string
  default     = "../../go/build/jmap-push/bootstrap.zip"
}

variable "search_indexer_zip" {
  description = "Path to the search-indexer Lambda zip built by make build."
  type        = string
  default     = "../../go/build/search-indexer/bootstrap.zip"
}

variable "outbox_dispatcher_zip" {
  description = "Path to the outbox-dispatcher Lambda zip built by make build."
  type        = string
  default     = "../../go/build/outbox-dispatcher/bootstrap.zip"
}

variable "submission_worker_zip" {
  description = "Path to the submission-worker Lambda zip built by make build."
  type        = string
  default     = "../../go/build/submission-worker/bootstrap.zip"
}

variable "submission_recovery_zip" {
  description = "Path to the submission-recovery Lambda zip built by make build."
  type        = string
  default     = "../../go/build/submission-recovery/bootstrap.zip"
}

variable "relay_webhook_zip" {
  description = "Path to the relay-webhook Lambda zip built by make build."
  type        = string
  default     = "../../go/build/relay-webhook/bootstrap.zip"
}

variable "delivery_worker_zip" {
  description = "Path to the delivery-worker Lambda zip built by make build."
  type        = string
  default     = "../../go/build/delivery-worker/bootstrap.zip"
}

variable "smtp2go_cnames" {
  description = "SMTP2GO sender-domain CNAMEs (DKIM and return path) from its dashboard (set in terraform.tfvars)."
  type = list(object({
    name    = string
    target  = string
    purpose = string
  }))
  default = []
}

variable "relay_quota" {
  description = <<-EOT
    The relay's sending caps, reserved atomically per submission. Defaults
    are SMTP2GO's free plan; verify the plan's limits, recipient accounting
    and reset timezone (docs/specs/mail-flow.md#outbound-relay-interface).
  EOT
  type = object({
    daily         = number
    monthly       = number
    per_recipient = bool
    timezone      = string
  })
  default = { daily = 200, monthly = 1000, per_recipient = true, timezone = "UTC" }
}

variable "search_zip" {
  description = "Path to the search Lambda zip built by make build."
  type        = string
  default     = "../../go/build/search/bootstrap.zip"
}
