# Alarms and the dashboard (docs/operations/monitoring.md#alarms-and-dashboard).
#
# Alarms have no notification actions: the owner chose not to configure an
# alert channel, so the dashboard's alarm panel is where they show. Every
# alarm uses built-in metrics except the unresolved-submission gauge,
# which the work-recovery sweep logs in Embedded Metric Format.

locals {
  region = data.aws_region.current.region

  queue_names    = { for k, q in var.queues : k => element(split(":", q.arn), 5) }
  dead_letter    = { for k, q in var.queues : k => local.queue_names[k] if q.dead_letter }
  backlog        = { for k, q in var.queues : k => q if !q.dead_letter && q.max_age_seconds != null }
  all_functions  = sort(flatten(values(var.functions)))
  unknown_metric = [var.metric_namespace, "SubmissionsUnknown", "component", var.recovery_component]
  period         = 300
}

data "aws_region" "current" {}

# --- Alarms ------------------------------------------------------------------

# Any dead-lettered message needs the owner's attention; the redrive
# policies say where each one can go back to.
resource "aws_cloudwatch_metric_alarm" "dead_letter" {
  for_each = local.dead_letter

  alarm_name          = "${var.name}-${each.key}-dead-letter"
  alarm_description   = "Messages in ${each.value}; inspect, fix and redrive (docs/operations/monitoring.md#failure-handling)."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = each.value }
  statistic           = "Maximum"
  period              = local.period
  evaluation_periods  = 1
  comparison_operator = "GreaterThanThreshold"
  threshold           = 0
  treat_missing_data  = "notBreaching"
}

# Work that waits longer than its queue's normal retries allow.
resource "aws_cloudwatch_metric_alarm" "backlog" {
  for_each = local.backlog

  alarm_name          = "${var.name}-${each.key}-backlog"
  alarm_description   = "The oldest message in ${local.queue_names[each.key]} is older than ${each.value.max_age_seconds} s."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  dimensions          = { QueueName = local.queue_names[each.key] }
  statistic           = "Maximum"
  period              = local.period
  evaluation_periods  = 1
  comparison_operator = "GreaterThanThreshold"
  threshold           = each.value.max_age_seconds
  treat_missing_data  = "notBreaching"
}

resource "aws_cloudwatch_metric_alarm" "errors" {
  for_each = var.functions

  alarm_name          = "${var.name}-${each.key}-errors"
  alarm_description   = "Function errors in ${join(", ", each.value)}; the dashboard shows which."
  evaluation_periods  = 1
  comparison_operator = "GreaterThanThreshold"
  threshold           = 0
  treat_missing_data  = "notBreaching"

  metric_query {
    id          = "total"
    expression  = "SUM(METRICS())"
    label       = "Errors"
    return_data = true
  }

  dynamic "metric_query" {
    for_each = each.value

    content {
      id = "f${metric_query.key}"

      metric {
        namespace   = "AWS/Lambda"
        metric_name = "Errors"
        dimensions  = { FunctionName = metric_query.value }
        stat        = "Sum"
        period      = local.period
      }
    }
  }
}

resource "aws_cloudwatch_metric_alarm" "api_5xx" {
  alarm_name          = "${var.name}-api-5xx"
  alarm_description   = "Server errors from the ${var.api.name} API."
  namespace           = "AWS/ApiGateway"
  metric_name         = "5XXError"
  dimensions          = { ApiName = var.api.name, Stage = var.api.stage }
  statistic           = "Sum"
  period              = local.period
  evaluation_periods  = 1
  comparison_operator = "GreaterThanThreshold"
  threshold           = 0
  treat_missing_data  = "notBreaching"
}

# Raised while any submission waits for the owner to resolve unknown
# relay acceptance (mail-admin submissions), and also when the one-minute
# sweep stops reporting, since a stopped sweep leaves leases unsettled.
resource "aws_cloudwatch_metric_alarm" "submissions_unknown" {
  alarm_name          = "${var.name}-submissions-unknown"
  alarm_description   = "A submission has unknown relay acceptance (resolve with mail-admin), or the work-recovery sweep stopped reporting."
  namespace           = local.unknown_metric[0]
  metric_name         = local.unknown_metric[1]
  dimensions          = { (local.unknown_metric[2]) = local.unknown_metric[3] }
  statistic           = "Maximum"
  period              = local.period
  evaluation_periods  = 1
  comparison_operator = "GreaterThanThreshold"
  threshold           = 0
  treat_missing_data  = "breaching"
}

# --- Dashboard ---------------------------------------------------------------

locals {
  alarm_arns = concat(
    [for a in aws_cloudwatch_metric_alarm.dead_letter : a.arn],
    [for a in aws_cloudwatch_metric_alarm.backlog : a.arn],
    [for a in aws_cloudwatch_metric_alarm.errors : a.arn],
    [aws_cloudwatch_metric_alarm.api_5xx.arn, aws_cloudwatch_metric_alarm.submissions_unknown.arn],
  )

  graph = { view = "timeSeries", region = local.region, period = local.period }

  widgets = [
    { type = "alarm", width = 24, height = 4, properties = { title = "Alarms", alarms = local.alarm_arns } },
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
        title   = "Queued messages", stat = "Maximum",
        metrics = [for k in sort(keys(var.queues)) : ["AWS/SQS", "ApproximateNumberOfMessagesVisible", "QueueName", local.queue_names[k]]]
      })
    },
    {
      type = "metric", width = 12, height = 6,
      properties = merge(local.graph, {
        title   = "Oldest message age (s)", stat = "Maximum",
        metrics = [for k in sort(keys(local.backlog)) : ["AWS/SQS", "ApproximateAgeOfOldestMessage", "QueueName", local.queue_names[k]]]
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
    {
      type = "metric", width = 12, height = 6,
      properties = merge(local.graph, {
        title   = "Submissions with unknown acceptance", stat = "Maximum",
        metrics = [local.unknown_metric]
      })
    },
  ]
}

resource "aws_cloudwatch_dashboard" "this" {
  dashboard_name = var.name
  dashboard_body = jsonencode({ widgets = local.widgets })
}
