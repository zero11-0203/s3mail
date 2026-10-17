# Offline checks of the outbound least-privilege and pause contracts
# (docs/specs/mail-flow.md#outbound-email, docs/operations/permissions.md).

mock_provider "aws" {
  mock_resource "aws_lambda_alias" {
    defaults = { arn = "arn:aws:lambda:us-east-1:111111111111:function:s3mail-outbound:live" }
  }

  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::111111111111:role/s3mail/s3mail-outbound" }
  }

  mock_resource "aws_cloudwatch_log_group" {
    defaults = { arn = "arn:aws:logs:us-east-1:111111111111:log-group:/aws/lambda/s3mail-outbound" }
  }

  mock_resource "aws_cloudwatch_event_rule" {
    defaults = { arn = "arn:aws:events:us-east-1:111111111111:rule/s3mail-outbound" }
  }

  mock_resource "aws_sqs_queue" {
    defaults = { arn = "arn:aws:sqs:us-east-1:111111111111:s3mail-submit", url = "https://sqs.us-east-1.amazonaws.com/111111111111/s3mail-submit" }
  }

  mock_resource "aws_secretsmanager_secret" {
    defaults = { arn = "arn:aws:secretsmanager:us-east-1:111111111111:secret:s3mail/relay/smtp2go-AbCdEf" }
  }

  mock_resource "aws_ssm_parameter" {
    defaults = { arn = "arn:aws:ssm:us-east-1:111111111111:parameter/s3mail/relay" }
  }
}

variables {
  dispatcher_zip       = "tests/fake-lambda.txt"
  worker_zip           = "tests/fake-lambda.txt"
  recovery_zip         = "tests/fake-lambda.txt"
  domain               = "example.com"
  relay                = { name = "smtp2go", smtp_host = "mail.example.net", smtp_port = 587 }
  table_name           = "s3mail-mail"
  table_arn            = "arn:aws:dynamodb:us-east-1:111111111111:table/s3mail-mail"
  stream_arn           = "arn:aws:dynamodb:us-east-1:111111111111:table/s3mail-mail/stream/2026-10-17T00:00:00.000"
  account_id           = "primary"
  account_partition    = "ACCOUNT#primary"
  originals_bucket_arn = "arn:aws:s3:::s3mail-originals-x"
  index_queue_url      = "https://sqs.us-east-1.amazonaws.com/111111111111/s3mail-index"
  index_queue_arn      = "arn:aws:sqs:us-east-1:111111111111:s3mail-index"
}

run "least_privilege" {
  command = apply

  assert {
    condition     = aws_ssm_parameter.relay.value == "paused"
    error_message = "sending starts paused"
  }

  assert {
    condition = alltrue([for k, p in aws_iam_role_policy.fn : alltrue([
      for st in jsondecode(p.policy).Statement : !anytrue([
        for a in flatten([st.Action]) : contains(["s3:DeleteObject", "s3:DeleteObjectVersion", "s3:PutObject", "s3:*", "dynamodb:*", "secretsmanager:*", "ssm:PutParameter"], a)
      ])
    ])])
    error_message = "no outbound role writes or deletes originals or changes its configuration"
  }

  assert {
    condition = alltrue([for k in ["worker", "recovery"] : alltrue([
      for st in jsondecode(aws_iam_role_policy.fn[k].policy).Statement : !contains(flatten([st.Action]), "dynamodb:DeleteItem")
    ])])
    error_message = "only the dispatcher deletes, and only to acknowledge outbox entries"
  }

  assert {
    condition = alltrue([for k in ["dispatcher", "recovery"] : alltrue([
      for st in jsondecode(aws_iam_role_policy.fn[k].policy).Statement :
      !anytrue([for a in flatten([st.Action]) : startswith(a, "secretsmanager:") || startswith(a, "s3:")])
    ])])
    error_message = "only the worker reads the relay secret or mail content"
  }

  assert {
    condition = alltrue([for k, p in aws_iam_role_policy.fn : alltrue([
      for st in jsondecode(p.policy).Statement :
      !contains(["dynamodb:GetItem", "dynamodb:Query", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem"], flatten([st.Action])[0]) ||
      st.Condition["ForAllValues:StringEquals"]["dynamodb:LeadingKeys"] == ["ACCOUNT#primary"]
    ])])
    error_message = "table access is limited to the account partition"
  }

  assert {
    condition     = aws_lambda_function.fn["worker"].timeout < 120
    error_message = "the worker's timeout stays below the two-minute attempt lease"
  }

  assert {
    condition     = jsondecode(one(one(aws_lambda_event_source_mapping.outbox.filter_criteria).filter).pattern).dynamodb.Keys.SK.S[0].prefix == "OUTBOX#"
    error_message = "the stream trigger is filtered to outbox inserts"
  }
}

run "rejects_plaintext_port" {
  command = plan

  variables {
    relay = { name = "smtp2go", smtp_host = "mail.example.net", smtp_port = 25 }
  }

  expect_failures = [var.relay]
}
