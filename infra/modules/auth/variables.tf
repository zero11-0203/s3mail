variable "name" {
  description = "User pool name."
  type        = string
  default     = "s3mail"
}

variable "domain_prefix" {
  description = "Globally unique Cognito domain prefix for the managed login page."
  type        = string
}

variable "terminal_callback_urls" {
  description = "Loopback redirect URIs for mail-auth (exact match; Cognito allows http only for localhost)."
  type        = list(string)
}
