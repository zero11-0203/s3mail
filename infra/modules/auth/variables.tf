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

variable "webmail_callback_urls" {
  description = "Browser webmail redirect URIs (exact match); empty registers no webmail app client."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for u in var.webmail_callback_urls : startswith(u, "https://")])
    error_message = "Webmail callback URLs use https."
  }
}

variable "agent_callback_urls" {
  description = "The mail agent's token-vault callback (infra/agent output callback_url); empty registers no agent app client."
  type        = list(string)
  default     = []

  validation {
    condition     = length(var.agent_callback_urls) <= 1 && alltrue([for u in var.agent_callback_urls : startswith(u, "https://")])
    error_message = "The agent client has one https redirect URI: the token-vault callback."
  }
}

variable "agent_scopes" {
  description = "Scopes of the agent client by agent phase (docs/specs/agent.md#identity-and-authorization): mail.read in A0, mail.write from A2, mail.submit from A3."
  type        = list(string)
  default     = ["mail.read"]

  validation {
    condition     = contains(var.agent_scopes, "mail.read") && alltrue([for s in var.agent_scopes : contains(["mail.read", "mail.write", "mail.submit"], s)])
    error_message = "agent_scopes includes mail.read and only s3mail scope names."
  }
}
