output "dashboard_url" {
  description = "The CloudWatch dashboard, whose alarm panel shows every alarm."
  value       = "https://${local.region}.console.aws.amazon.com/cloudwatch/home?region=${local.region}#dashboards/dashboard/${aws_cloudwatch_dashboard.this.dashboard_name}"
}

output "alarm_names" {
  value = sort(concat(
    [for a in aws_cloudwatch_metric_alarm.dead_letter : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.backlog : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.errors : a.alarm_name],
    [aws_cloudwatch_metric_alarm.api_5xx.alarm_name, aws_cloudwatch_metric_alarm.submissions_unknown.alarm_name],
  ))
}
