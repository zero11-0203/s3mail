variable "name" {
  description = "Prefix for the function, role, schedule and log group."
  type        = string
  default     = "s3mail"
}

variable "gc_zip" {
  description = "Path to the gc Lambda zip (make build)."
  type        = string
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

variable "index_bucket" {
  description = "The search index bucket (module.search.index_bucket); the only bucket the collector touches."
  type        = string
}

variable "schedule" {
  description = "When the collector runs."
  type        = string
  default     = "cron(30 3 * * ? *)"
}

variable "enabled" {
  description = "false pauses the schedule, for example during a rebuild (docs/operations/recovery.md#restore-runbook)."
  type        = bool
  default     = true
}

variable "log_retention_days" {
  type    = number
  default = 30
}
