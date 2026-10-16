variable "name" {
  description = "Name for the Lambda, role, log group and API."
  type        = string
  default     = "s3mail-spike-stream"
}

variable "lambda_zip" {
  description = "Path to the built Lambda zip (make spike-build)."
  type        = string
}

variable "authorizer_zip" {
  description = "Path to the built authorizer Lambda zip (make spike-build)."
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
