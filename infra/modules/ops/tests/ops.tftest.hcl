# Offline checks of the dashboard (docs/operations/monitoring.md#acceptance-checks).

mock_provider "aws" {
  mock_data "aws_region" {
    defaults = { region = "us-east-1" }
  }
}

variables {
  queues = {
    ingest     = { arn = "arn:aws:sqs:us-east-1:111111111111:s3mail-ingest" }
    ingest-dlq = { arn = "arn:aws:sqs:us-east-1:111111111111:s3mail-ingest-dlq", dead_letter = true }
  }
  functions = {
    inbound  = ["s3mail-ingest"]
    outbound = ["s3mail-outbound-worker", "s3mail-outbound-recovery"]
  }
  api = { name = "s3mail-jmap", stage = "v1" }
}

run "dashboard" {
  command = apply

  assert {
    condition     = sum([for w in jsondecode(aws_cloudwatch_dashboard.this.dashboard_body).widgets : length(w.properties.metrics)]) <= 50
    error_message = "the dashboard stays within the free metric count"
  }

  assert {
    condition     = jsondecode(aws_cloudwatch_dashboard.this.dashboard_body).widgets[2].properties.metrics[1][3] == "s3mail-ingest-dlq"
    error_message = "queue depth includes the dead-letter queues"
  }
}
