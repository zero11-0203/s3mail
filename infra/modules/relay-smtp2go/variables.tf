variable "smtp_host" {
  type    = string
  default = "mail.smtp2go.com"
}

variable "smtp_port" {
  description = "587 (required STARTTLS) or 465 (implicit TLS)."
  type        = number
  default     = 587
}

variable "cnames" {
  description = "Sender-domain CNAMEs from the SMTP2GO dashboard, names relative to the zone."
  type = list(object({
    name    = string
    target  = string
    purpose = string
  }))
  default = []

  validation {
    condition     = alltrue([for r in var.cnames : r.name != "" && !endswith(r.name, ".")])
    error_message = "CNAME names are relative to the zone and cannot be the apex."
  }
}

variable "dns_ttl" {
  type    = number
  default = 300
}
