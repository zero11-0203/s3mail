variable "name" {
  description = "Prefix for resource names."
  type        = string
  default     = "s3mail"
}

variable "account_id" {
  description = "Internal JMAP account id; the table partition is ACCOUNT#<account_id>."
  type        = string
  default     = "primary"

  validation {
    condition     = can(regex("^[a-z0-9-]{1,32}$", var.account_id))
    error_message = "account_id is 1-32 lowercase letters, digits or dashes."
  }
}
