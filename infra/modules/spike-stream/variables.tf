variable "name" {
  description = "Name for the Lambda, role, log group and API."
  type        = string
  default     = "s3mail-spike-stream"
}

variable "lambda_zip" {
  description = "Path to the built Lambda zip (make spike-build)."
  type        = string
}
