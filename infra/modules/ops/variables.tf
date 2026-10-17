variable "name" {
  description = "Dashboard name."
  type        = string
  default     = "s3mail"
}

variable "queues" {
  description = "Queues by short name; dead-letter queues are shown by depth only."
  type = map(object({
    arn         = string
    dead_letter = optional(bool, false)
  }))
}

variable "functions" {
  description = "Lambda function names by group."
  type        = map(list(string))
}

variable "api" {
  description = "The REST API name and stage."
  type = object({
    name  = string
    stage = string
  })
}
