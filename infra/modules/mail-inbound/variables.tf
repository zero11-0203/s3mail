variable "name" {
  description = "Prefix for resource names."
  type        = string
  default     = "s3mail"
}

variable "domain" {
  description = "Domain SES receives for; the MX and verification records go in its zone."
  type        = string

  validation {
    condition     = can(regex("^([a-z0-9]([a-z0-9-]*[a-z0-9])?\\.)+[a-z]{2,63}$", var.domain))
    error_message = "domain must be a lowercase domain name without a trailing dot."
  }
}

variable "aliases" {
  description = "Local parts that receive mail (set in terraform.tfvars). The reserved canary address is always added; +tag variants are accepted by SES."
  type        = list(string)

  validation {
    condition     = alltrue([for a in var.aliases : can(regex("^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$", a))])
    error_message = "Aliases are lowercase local parts without '+' or '@'."
  }

  validation {
    condition     = length(distinct(concat(var.aliases, ["canary"]))) <= 100
    error_message = "One SES receipt rule holds at most 100 recipients."
  }
}

variable "ses_key_prefix" {
  description = "Key prefix for SES deliveries in the original bucket."
  type        = string
  default     = "inbound/"

  validation {
    condition     = can(regex("^[a-z0-9-]+/$", var.ses_key_prefix))
    error_message = "ses_key_prefix is one lowercase path segment ending in '/'."
  }
}

variable "dmarc_policy" {
  description = "DMARC TXT value for the domain; p=reject until an aligned relay signs outbound mail."
  type        = string
  default     = "v=DMARC1; p=reject"

  validation {
    condition     = startswith(var.dmarc_policy, "v=DMARC1;") && !strcontains(var.dmarc_policy, "\"")
    error_message = "dmarc_policy must start with 'v=DMARC1;' and contain no quotes."
  }
}

variable "ingest_timeout_seconds" {
  description = "Planned processing Lambda timeout; the queue visibility timeout is six times this."
  type        = number
  default     = 90
}

variable "max_receive_count" {
  description = "Receives before an ingestion message moves to the dead-letter queue."
  type        = number
  default     = 5
}

variable "dns_ttl" {
  description = "TTL for the declared records."
  type        = number
  default     = 300
}
