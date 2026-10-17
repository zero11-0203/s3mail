# The operations dashboard (docs/operations/monitoring.md#alarms-and-dashboard).
#
# Built-in metrics only, and no alarms or custom metrics: the owner chose
# to avoid their cost. One dashboard of up to 50 metrics is free.

locals {
  region = data.aws_region.current.region

  queue_names   = { for k, q in var.queues : k => element(split(":", q.arn), 5) }
  work_queues   = sort([for k, q in var.queues : k if !q.dead_letter])
  all_functions = sort(flatten(values(var.functions)))
  period        = 300

  graph = { view = "timeSeries", region = local.region, period = local.period }

  widgets = [
    {
      type = "metric", width = 12, height = 6,
      properties = merge(local.graph, {
        title   = "Function errors", stat = "Sum",
        metrics = [for f in local.all_functions : ["AWS/Lambda", "Errors", "FunctionName", f]]
      })
    },
    {
      type = "metric", width = 12, height = 6,
      properties = merge(local.graph, {
        title   = "Function throttles", stat = "Sum",
        metrics = [for f in local.all_functions : ["AWS/Lambda", "Throttles", "FunctionName", f]]
      })
    },
    {
      type = "metric", width = 12, height = 6,
      properties = merge(local.graph, {
        title   = "Queued messages (dead-letter queues should stay at zero)", stat = "Maximum",
        metrics = [for k in sort(keys(var.queues)) : ["AWS/SQS", "ApproximateNumberOfMessagesVisible", "QueueName", local.queue_names[k]]]
      })
    },
    {
      type = "metric", width = 12, height = 6,
      properties = merge(local.graph, {
        title   = "Oldest message age (s)", stat = "Maximum",
        metrics = [for k in local.work_queues : ["AWS/SQS", "ApproximateAgeOfOldestMessage", "QueueName", local.queue_names[k]]]
      })
    },
    {
      type = "metric", width = 12, height = 6,
      properties = merge(local.graph, {
        title   = "API requests and errors", stat = "Sum",
        metrics = [for m in ["Count", "4XXError", "5XXError"] : ["AWS/ApiGateway", m, "ApiName", var.api.name, "Stage", var.api.stage]]
      })
    },
    {
      type = "metric", width = 12, height = 6,
      properties = merge(local.graph, {
        title   = "API latency (ms)",
        metrics = [for s in ["p50", "p95"] : ["AWS/ApiGateway", "Latency", "ApiName", var.api.name, "Stage", var.api.stage, { stat = s }]]
      })
    },
  ]
}

data "aws_region" "current" {}

resource "aws_cloudwatch_dashboard" "this" {
  dashboard_name = var.name
  dashboard_body = jsonencode({ widgets = local.widgets })
}
