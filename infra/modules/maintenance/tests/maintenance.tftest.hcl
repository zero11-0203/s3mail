# Offline checks of the collector's least-privilege contract
# (docs/operations/recovery.md#acceptance-checks).

mock_provider "aws" {
  mock_resource "aws_lambda_alias" {
    defaults = { arn = "arn:aws:lambda:us-east-1:111111111111:function:s3mail-gc:live" }
  }

  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::111111111111:role/s3mail/s3mail-gc" }
  }

  mock_resource "aws_cloudwatch_log_group" {
    defaults = { arn = "arn:aws:logs:us-east-1:111111111111:log-group:/aws/lambda/s3mail-gc" }
  }

  mock_resource "aws_cloudwatch_event_rule" {
    defaults = { arn = "arn:aws:events:us-east-1:111111111111:rule/s3mail-gc" }
  }
}

variables {
  gc_zip            = "tests/fake-lambda.txt"
  table_name        = "s3mail-mail"
  table_arn         = "arn:aws:dynamodb:us-east-1:111111111111:table/s3mail-mail"
  account_id        = "primary"
  account_partition = "ACCOUNT#primary"
  index_bucket      = "s3mail-index-x"
}

run "least_privilege" {
  command = apply

  assert {
    condition = alltrue([
      for st in jsondecode(aws_iam_role_policy.gc.policy).Statement : alltrue([
        for r in flatten([st.Resource]) : r == "*" || startswith(r, "arn:aws:logs:") || startswith(r, "arn:aws:dynamodb:") || startswith(r, "arn:aws:s3:::s3mail-index-x")
      ])
    ])
    error_message = "the collector reaches only its logs, the table and the index bucket, never the original-mail bucket"
  }

  assert {
    condition = alltrue([
      for st in jsondecode(aws_iam_role_policy.gc.policy).Statement : !anytrue([
        for a in flatten([st.Action]) : contains(["s3:*", "dynamodb:*", "s3:DeleteObject", "s3:PutObject", "s3:GetObject", "s3:GetObjectVersion", "dynamodb:PutItem", "dynamodb:BatchWriteItem", "s3:PutLifecycleConfiguration"], a)
      ])
    ])
    error_message = "the collector neither writes nor reads objects and cannot add delete markers"
  }

  assert {
    condition = alltrue([
      for st in jsondecode(aws_iam_role_policy.gc.policy).Statement : st.Sid != "PurgeIndexVersions" || st.Resource == "arn:aws:s3:::s3mail-index-x/index/*"
    ])
    error_message = "version deletion is limited to index snapshots"
  }

  assert {
    condition = alltrue([
      for st in jsondecode(aws_iam_role_policy.gc.policy).Statement : !startswith(tostring(try(st.Resource, "")), "arn:aws:dynamodb:") || st.Condition["ForAllValues:StringEquals"]["dynamodb:LeadingKeys"] == ["ACCOUNT#primary"]
    ])
    error_message = "table access is limited to the account partition"
  }

  assert {
    condition     = aws_lambda_function.gc.reserved_concurrent_executions == 1 && one(aws_lambda_function.gc.tracing_config).mode == "Active"
    error_message = "one traced collector at a time"
  }

  assert {
    condition     = aws_cloudwatch_event_rule.gc.state == "ENABLED"
    error_message = "the collector runs on schedule"
  }
}

run "paused" {
  command = plan

  variables {
    enabled = false
  }

  assert {
    condition     = aws_cloudwatch_event_rule.gc.state == "DISABLED"
    error_message = "enabled = false pauses the collector"
  }
}
