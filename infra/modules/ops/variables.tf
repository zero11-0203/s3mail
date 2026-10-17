variable "name" {
  description = "Prefix for the alarms and the dashboard."
  type        = string
  default     = "s3mail"
}

variable "queues" {
  description = "Queues by short name: dead_letter queues alarm on any message, others on max_age_seconds when set."
  type = map(object({
    arn             = string
    dead_letter     = optional(bool, false)
    max_age_seconds = optional(number)
  }))
}

variable "functions" {
  description = "Lambda function names by group; each group has one error alarm."
  type        = map(list(string))

  validation {
    # One metric math alarm holds at most ten metrics, the sum included.
    condition     = alltrue([for g in values(var.functions) : length(g) >= 1 && length(g) <= 9])
    error_message = "Each function group needs one to nine functions."
  }
}

variable "api" {
  description = "The REST API name and stage, for the 5XX alarm."
  type = object({
    name  = string
    stage = string
  })
}

variable "recovery_component" {
  description = "The component dimension of the work-recovery sweep's SubmissionsUnknown metric."
  type        = string
  default     = "submission-recovery"
}

variable "metric_namespace" {
  description = "Namespace of the custom mail metrics (send.MetricNamespace)."
  type        = string
  default     = "Mail"
}
