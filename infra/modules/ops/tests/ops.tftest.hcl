# Offline checks of the alarm set (docs/operations/monitoring.md#acceptance-checks).

mock_provider "aws" {
  mock_data "aws_region" {
    defaults = { region = "us-east-1" }
  }
}

variables {
  queues = {
    ingest     = { arn = "arn:aws:sqs:us-east-1:111111111111:s3mail-ingest", max_age_seconds = 900 }
    submit     = { arn = "arn:aws:sqs:us-east-1:111111111111:s3mail-submit" }
    ingest-dlq = { arn = "arn:aws:sqs:us-east-1:111111111111:s3mail-ingest-dlq", dead_letter = true }
    submit-dlq = { arn = "arn:aws:sqs:us-east-1:111111111111:s3mail-submit-dlq", dead_letter = true }
  }
  functions = {
    inbound  = ["s3mail-ingest"]
    outbound = ["s3mail-outbound-worker", "s3mail-outbound-recovery"]
  }
  api = { name = "s3mail-jmap", stage = "v1" }
}

run "alarms" {
  command = apply

  assert {
    condition     = toset(keys(aws_cloudwatch_metric_alarm.dead_letter)) == toset(["ingest-dlq", "submit-dlq"]) && aws_cloudwatch_metric_alarm.dead_letter["submit-dlq"].dimensions.QueueName == "s3mail-submit-dlq"
    error_message = "every dead-letter queue has an alarm on its own queue"
  }

  assert {
    condition     = alltrue([for a in aws_cloudwatch_metric_alarm.dead_letter : a.threshold == 0 && a.comparison_operator == "GreaterThanThreshold"])
    error_message = "one dead-lettered message raises the alarm"
  }

  assert {
    condition     = keys(aws_cloudwatch_metric_alarm.backlog) == ["ingest"] && aws_cloudwatch_metric_alarm.backlog["ingest"].threshold == 900
    error_message = "only queues with a maximum age have a backlog alarm"
  }

  assert {
    condition     = length(aws_cloudwatch_metric_alarm.errors["outbound"].metric_query) == 3
    error_message = "a group's error alarm sums each of its functions"
  }

  assert {
    condition     = aws_cloudwatch_metric_alarm.submissions_unknown.treat_missing_data == "breaching" && aws_cloudwatch_metric_alarm.submissions_unknown.namespace == "Mail"
    error_message = "unknown acceptance alarms, and so does a stopped recovery sweep"
  }

  assert {
    condition = alltrue([for a in concat(values(aws_cloudwatch_metric_alarm.dead_letter), values(aws_cloudwatch_metric_alarm.backlog), values(aws_cloudwatch_metric_alarm.errors), [aws_cloudwatch_metric_alarm.api_5xx, aws_cloudwatch_metric_alarm.submissions_unknown]) :
    length(coalesce(a.alarm_actions, [])) == 0])
    error_message = "no notification channel is configured"
  }

  assert {
    condition     = length(output.alarm_names) == 7
    error_message = "the dashboard lists every alarm"
  }
}

run "rejects_large_function_group" {
  command = plan

  variables {
    functions = { all = [for i in range(10) : "f${i}"] }
  }

  expect_failures = [var.functions]
}
