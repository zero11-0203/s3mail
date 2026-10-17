variable "name" {
  description = "Prefix for the functions, roles, queues, secret, parameter and log groups."
  type        = string
  default     = "s3mail"
}

variable "dispatcher_zip" {
  description = "Path to the outbox-dispatcher Lambda zip (make build)."
  type        = string
}

variable "worker_zip" {
  description = "Path to the submission-worker Lambda zip (make build)."
  type        = string
}

variable "recovery_zip" {
  description = "Path to the submission-recovery Lambda zip (make build)."
  type        = string
}

variable "webhook_zip" {
  description = "Path to the relay-webhook Lambda zip (make build)."
  type        = string
}

variable "delivery_zip" {
  description = "Path to the delivery-worker Lambda zip (make build)."
  type        = string
}

variable "webhook_concurrency" {
  description = "Reserved concurrency of the public webhook receiver."
  type        = number
  default     = 2
}

variable "domain" {
  description = "Mail domain; the worker's SMTP HELO name."
  type        = string
}

variable "relay" {
  description = "The active relay adapter's configuration (a relay-* module's relay output)."
  type = object({
    name      = string
    smtp_host = string
    smtp_port = number
  })

  validation {
    condition     = contains([465, 587], var.relay.smtp_port)
    error_message = "The smtp adapter uses implicit TLS on 465 or required STARTTLS on 587."
  }
}

variable "table_name" {
  type = string
}

variable "table_arn" {
  type = string
}

variable "stream_arn" {
  description = "The mail table's KEYS_ONLY stream."
  type        = string
}

variable "account_id" {
  description = "Internal account id (module.mail_store.account_id)."
  type        = string
}

variable "account_partition" {
  description = "The account's partition key, for dynamodb:LeadingKeys."
  type        = string
}

variable "originals_bucket_arn" {
  description = "Permanent original-mail bucket; the worker reads exact composed/imported versions."
  type        = string
}

variable "index_queue_url" {
  type = string
}

variable "index_queue_arn" {
  type = string
}

variable "worker_concurrency" {
  description = "Reserved concurrency of the submission worker."
  type        = number
  default     = 2
}

variable "sweep_schedule" {
  description = "How often the dispatcher sweeps the outbox and checks the index."
  type        = string
  default     = "rate(5 minutes)"
}

variable "recovery_schedule" {
  description = "How often work recovery runs (docs/specs/mail-flow.md#outbound-email)."
  type        = string
  default     = "rate(1 minute)"
}

variable "log_retention_days" {
  type    = number
  default = 30
}
