variable "region" {
  description = "AWS region for every project resource; must be an SES email receiving region (docs/operations/deployment.md#dns-and-region)."
  type        = string
}

variable "project" {
  description = "Project name used for resource names and tags."
  type        = string
  default     = "s3mail"
}
