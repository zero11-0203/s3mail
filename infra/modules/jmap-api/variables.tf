variable "name" {
  description = "Prefix for the API, functions, roles and log groups."
  type        = string
  default     = "s3mail"
}

variable "stage_name" {
  description = "API stage; it is the first path segment of every URL until a custom domain is mapped."
  type        = string
  default     = "v1"
}

variable "api_zip" {
  description = "Path to the jmap-api Lambda zip (make build)."
  type        = string
}

variable "download_zip" {
  description = "Path to the jmap-download Lambda zip (make build)."
  type        = string
}

variable "push_zip" {
  description = "Path to the jmap-push Lambda zip (make build)."
  type        = string
}

variable "authorizer_zip" {
  description = "Path to the bearer-token authorizer Lambda zip (make build)."
  type        = string
}

variable "issuer" {
  description = "Cognito issuer the authorizer trusts (module.auth.issuer)."
  type        = string
}

variable "client_ids" {
  description = "Allowlisted Cognito app client IDs."
  type        = list(string)
}

variable "scopes" {
  description = "Wire scopes advertised in the protected resource metadata."
  type        = list(string)
}

variable "owner_subject" {
  description = "Cognito sub mapped to the account (docs/specs/security.md#browser-login)."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-f-]{36}$", var.owner_subject))
    error_message = "owner_subject is the owner's Cognito sub (a UUID)."
  }
}

variable "table_name" {
  type = string
}

variable "table_arn" {
  type = string
}

variable "account_id" {
  description = "Internal account id (module.mail_store.account_id)."
  type        = string
}

variable "account_partition" {
  description = "The account's partition key, for dynamodb:LeadingKeys."
  type        = string
}

variable "originals_bucket_name" {
  type        = string
  description = "Permanent original-mail bucket; imported and composed MIME goes under imported/ and composed/."
}

variable "originals_bucket_arn" {
  type = string
}

variable "ses_key_prefix" {
  description = "Key prefix of SES deliveries; JMAP roles read only these versions."
  type        = string
}

variable "search_function_arn" {
  description = "The search function's alias ARN; only the API function may invoke it. Null disables text search."
  type        = string
  default     = null
}

variable "semantic" {
  description = "Advertise SemanticEmail/query; only when the search function embeds (docs/specs/jmap.md#semantic-search)."
  type        = bool
  default     = false
}

variable "sending" {
  description = "Enables Identity and EmailSubmission on the API function. Null disables submission."
  type = object({
    domain        = string
    aliases       = list(string)
    relay         = string
    daily         = number
    monthly       = number
    per_recipient = bool
    timezone      = string
  })
  default = null
}

variable "webhook" {
  description = "The relay webhook receiver (mail-outbound's webhook output) served unauthenticated at POST /webhooks/<relay>; it checks its own bearer secret. Null serves no webhook."
  type = object({
    function_name = string
    alias_name    = string
    alias_arn     = string
    relay         = string
  })
  default = null
}

variable "cors_origin" {
  description = "The one browser origin allowed to call the API (the Phase 4 webmail, e.g. https://webmail.example.com). Null disables CORS."
  type        = string
  default     = null

  validation {
    condition     = var.cors_origin == null || can(regex("^https://[a-z0-9.-]+$", var.cors_origin))
    error_message = "cors_origin is an https origin: lowercase host, no port, path or trailing slash."
  }
}

variable "push_concurrency" {
  description = "Reserved concurrency of the push function: the cap on open EventSource streams."
  type        = number
  default     = 4
}

variable "log_retention_days" {
  type    = number
  default = 30
}
