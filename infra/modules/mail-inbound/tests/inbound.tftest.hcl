# Offline checks of the original-retention and receipt contracts
# (docs/operations/recovery.md#acceptance-checks, docs/specs/mail-flow.md#acceptance-checks).

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "111111111111" }
  }

  mock_data "aws_region" {
    defaults = { region = "us-east-1" }
  }

  mock_resource "aws_s3_bucket" {
    defaults = {
      id  = "s3mail-originals-test"
      arn = "arn:aws:s3:::s3mail-originals-test"
    }
  }

  mock_resource "aws_sns_topic" {
    defaults = { arn = "arn:aws:sns:us-east-1:111111111111:s3mail-inbound-receipts" }
  }

  mock_resource "aws_lambda_alias" {
    defaults = { arn = "arn:aws:lambda:us-east-1:111111111111:function:s3mail-ingest:live" }
  }

  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::111111111111:role/s3mail/s3mail-ingest" }
  }

  mock_resource "aws_ses_domain_identity" {
    defaults = { verification_token = "synthetic-verification-token" }
  }
}

variables {
  domain            = "example.com"
  aliases           = ["alice", "lists"]
  ingest_zip        = "tests/fake-lambda.txt"
  table_name        = "s3mail-mail"
  table_arn         = "arn:aws:dynamodb:us-east-1:111111111111:table/s3mail-mail"
  account_id        = "primary"
  account_partition = "ACCOUNT#primary"
}

run "originals_are_versioned_encrypted_and_undeletable" {
  command = apply

  assert {
    condition     = aws_s3_bucket_versioning.originals.versioning_configuration[0].status == "Enabled"
    error_message = "original bucket versioning must be enabled"
  }

  assert {
    condition     = one([for r in aws_s3_bucket_server_side_encryption_configuration.originals.rule : one(r.apply_server_side_encryption_by_default).sse_algorithm]) == "AES256"
    error_message = "originals use SSE-S3"
  }

  assert {
    condition     = aws_s3_bucket.originals.force_destroy != true
    error_message = "the original bucket is never force-emptied"
  }

  assert {
    condition = length(setsubtract(
      ["s3:DeleteObject", "s3:DeleteObjectVersion", "s3:DeleteBucket", "s3:PutLifecycleConfiguration", "s3:PutBucketVersioning"],
      flatten([for s in jsondecode(aws_s3_bucket_policy.originals.policy).Statement : s.Action if s.Effect == "Deny" && s.Principal == "*" && !can(s.Condition)])
    )) == 0
    error_message = "the bucket policy denies deletion, lifecycle and versioning changes to every principal"
  }

  assert {
    condition = alltrue([
      for s in jsondecode(aws_s3_bucket_policy.originals.policy).Statement :
      s.Effect == "Deny" || (s.Action == "s3:PutObject" && s.Condition.StringEquals["AWS:SourceAccount"] == "111111111111")
    ])
    error_message = "the only allow is SES PutObject from this account"
  }

  assert {
    condition     = aws_s3_bucket_public_access_block.originals.block_public_policy && aws_s3_bucket_public_access_block.originals.restrict_public_buckets
    error_message = "public access is blocked"
  }
}

run "receipt_rule_routes_declared_recipients_only" {
  command = apply

  assert {
    condition     = aws_ses_receipt_rule.deliver.recipients == toset(["alice@example.com", "canary@example.com", "lists@example.com"])
    error_message = "declared aliases plus the reserved canary, no catch-all"
  }

  assert {
    condition     = aws_ses_receipt_rule.deliver.scan_enabled
    error_message = "spam and virus scanning is enabled"
  }

  assert {
    condition     = one(aws_ses_receipt_rule.deliver.s3_action).topic_arn == aws_sns_topic.receipts.arn
    error_message = "the S3 action publishes its receipt notification"
  }

  assert {
    condition     = one(aws_ses_receipt_rule.deliver.s3_action).kms_key_arn == null
    error_message = "SES client-side encryption is not selected"
  }

  assert {
    condition     = one(aws_ses_receipt_rule.deliver.stop_action).scope == "RuleSet"
    error_message = "a stop action follows the delivery"
  }

  assert {
    condition     = aws_ses_active_receipt_rule_set.this.rule_set_name == aws_ses_receipt_rule_set.this.rule_set_name
    error_message = "the module's rule set is active"
  }
}

run "queues_follow_the_queue_contract" {
  command = apply

  assert {
    condition     = aws_sqs_queue.ingest.visibility_timeout_seconds >= 6 * var.ingest_timeout_seconds
    error_message = "visibility timeout is at least six Lambda timeouts"
  }

  assert {
    condition     = jsondecode(aws_sqs_queue.ingest.redrive_policy).maxReceiveCount == 5
    error_message = "messages reach the DLQ after five receives"
  }

  assert {
    condition     = aws_sns_topic_subscription.ingest.redrive_policy != null && aws_sns_topic_subscription.ingest.raw_message_delivery != true
    error_message = "SNS delivery failures go to a DLQ and the SNS envelope is kept"
  }

  assert {
    condition     = alltrue([aws_sqs_queue.ingest.sqs_managed_sse_enabled, aws_sqs_queue.ingest_dlq.sqs_managed_sse_enabled, aws_sqs_queue.delivery_dlq.sqs_managed_sse_enabled])
    error_message = "queues are encrypted at rest"
  }
}

run "declares_mx_verification_and_dmarc" {
  command = apply

  assert {
    condition     = toset([for r in output.dns_records : "${r.name} ${r.type}"]) == toset([" MX", "_amazonses TXT", "_dmarc TXT"])
    error_message = "MX, SES verification TXT and DMARC are declared"
  }

  assert {
    condition     = one([for r in output.dns_records : r.values if r.type == "MX"]) == ["10 inbound-smtp.us-east-1.amazonaws.com"]
    error_message = "MX points at the region's SES receiving endpoint"
  }

  assert {
    condition     = one([for r in output.dns_records : r.values if r.name == "_dmarc"]) == ["v=DMARC1; p=reject"]
    error_message = "DMARC defaults to reject"
  }
}

run "rejects_addresses_as_aliases" {
  command = plan

  variables {
    aliases = ["alice@example.com"]
  }

  expect_failures = [var.aliases]
}

run "rejects_tag_aliases" {
  command = plan

  variables {
    aliases = ["alice+news"]
  }

  expect_failures = [var.aliases]
}

run "ingest_role_is_least_privilege" {
  command = apply

  assert {
    condition = length([
      for st in jsondecode(aws_iam_role_policy.ingest.policy).Statement : st
      if length([for a in flatten([st.Action]) : a if can(regex("(?i)delete|put(object|bucket|lifecycle)|\\*", a)) && a != "sqs:DeleteMessage"]) > 0
    ]) == 0
    error_message = "the ingest role has no S3 write/delete grants and no action wildcards"
  }

  assert {
    condition = alltrue([
      for st in jsondecode(aws_iam_role_policy.ingest.policy).Statement :
      st.Resource != "*" || st.Sid == "WriteTraces"
    ])
    error_message = "only the X-Ray statement uses Resource *"
  }

  assert {
    condition = one([
      for st in jsondecode(aws_iam_role_policy.ingest.policy).Statement : st.Condition["ForAllValues:StringEquals"]["dynamodb:LeadingKeys"]
      if st.Sid == "CommitToOwnAccountPartition"
    ]) == ["ACCOUNT#primary"]
    error_message = "table access is limited to the account partition"
  }

  assert {
    condition     = aws_lambda_event_source_mapping.ingest.function_name == aws_lambda_alias.ingest_live.arn
    error_message = "the queue triggers the live alias"
  }

  assert {
    condition     = contains(aws_lambda_event_source_mapping.ingest.function_response_types, "ReportBatchItemFailures")
    error_message = "failures are reported per item"
  }

  assert {
    condition     = aws_sqs_queue.ingest.visibility_timeout_seconds >= 6 * aws_lambda_function.ingest.timeout
    error_message = "visibility timeout is at least six Lambda timeouts"
  }

  assert {
    condition     = aws_lambda_function.ingest.environment[0].variables.ALIASES == "alice,lists,canary"
    error_message = "the runtime registry comes from the same alias declaration as the receipt rule"
  }

  assert {
    condition     = aws_cloudwatch_log_group.ingest.retention_in_days == 30 && one(aws_lambda_function.ingest.tracing_config).mode == "Active"
    error_message = "30-day logs and active tracing"
  }
}
