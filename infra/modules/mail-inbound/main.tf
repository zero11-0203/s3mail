# SES receiving for one domain (docs/specs/mail-flow.md#inbound-email):
# SES stores each accepted message in the permanent original bucket and
# publishes the S3-action receipt notification (envelope recipients, verdicts,
# object key) to SNS, which fans out to the ingestion queue. No IAM role is
# created here; SES and SNS act through resource policies scoped to this
# account and the exact rule/topic.

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id
  region     = data.aws_region.current.region

  # Catch-all is disabled: only declared aliases and the reserved canary
  # receive. SES also accepts their +tag variants.
  local_parts = distinct(concat(var.aliases, ["canary"]))
  recipients  = sort([for p in local.local_parts : "${p}@${var.domain}"])

  rule_set_name = "${var.name}-inbound"
  rule_name     = "deliver"
  rule_arn      = "arn:aws:ses:${local.region}:${local.account_id}:receipt-rule-set/${local.rule_set_name}:receipt-rule/${local.rule_name}"

  ses_source = {
    StringEquals = { "AWS:SourceAccount" = local.account_id }
    ArnLike      = { "AWS:SourceArn" = local.rule_arn }
  }
}

# ---------------------------------------------------------------------------
# Permanent original bucket (docs/operations/recovery.md#original-mail-retention):
# versioned, SSE-S3, no lifecycle configuration and no delete path. The
# bucket policy denies object/version deletion and lifecycle or versioning
# changes to every principal, so even an administrator session cannot remove
# originals without first deleting the policy itself.

resource "aws_s3_bucket" "originals" {
  bucket_prefix = "${var.name}-originals-"

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_ownership_controls" "originals" {
  bucket = aws_s3_bucket.originals.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "originals" {
  bucket = aws_s3_bucket.originals.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "originals" {
  bucket = aws_s3_bucket.originals.id

  versioning_configuration {
    status = "Enabled"
  }
}

# Ordinary server-side encryption; the SES S3 action's optional client-side
# KMS mode is deliberately not used (docs/specs/mail-flow.md#inbound-email).
resource "aws_s3_bucket_server_side_encryption_configuration" "originals" {
  bucket = aws_s3_bucket.originals.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_policy" "originals" {
  bucket = aws_s3_bucket.originals.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "SesDeliversOriginals"
        Effect    = "Allow"
        Principal = { Service = "ses.amazonaws.com" }
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.originals.arn}/${var.ses_key_prefix}*"
        Condition = local.ses_source
      },
      {
        Sid       = "OriginalsAreNeverDeleted"
        Effect    = "Deny"
        Principal = "*"
        Action = [
          "s3:DeleteBucket",
          "s3:DeleteObject",
          "s3:DeleteObjectVersion",
          "s3:PutBucketVersioning",
          "s3:PutLifecycleConfiguration",
        ]
        Resource = [
          aws_s3_bucket.originals.arn,
          "${aws_s3_bucket.originals.arn}/*",
        ]
      },
      {
        Sid       = "TlsOnly"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:*"
        Resource = [
          aws_s3_bucket.originals.arn,
          "${aws_s3_bucket.originals.arn}/*",
        ]
        Condition = { Bool = { "aws:SecureTransport" = "false" } }
      },
    ]
  })

  # Versioning and public-access settings are written before the deny on
  # versioning changes takes effect.
  depends_on = [
    aws_s3_bucket_public_access_block.originals,
    aws_s3_bucket_versioning.originals,
  ]
}

# ---------------------------------------------------------------------------
# Receipt notification path: SES S3 action → SNS → SQS ingestion queue.

resource "aws_sns_topic" "receipts" {
  name = "${var.name}-inbound-receipts"
}

resource "aws_sns_topic_policy" "receipts" {
  arn = aws_sns_topic.receipts.arn
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "SesPublishesReceipts"
      Effect    = "Allow"
      Principal = { Service = "ses.amazonaws.com" }
      Action    = "SNS:Publish"
      Resource  = aws_sns_topic.receipts.arn
      Condition = local.ses_source
    }]
  })
}

# Retains receipts the processing Lambda could not handle after
# max_receive_count attempts (docs/operations/deployment.md#queue-configuration).
resource "aws_sqs_queue" "ingest_dlq" {
  name                      = "${var.name}-ingest-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
}

resource "aws_sqs_queue" "ingest" {
  name                       = "${var.name}-ingest"
  visibility_timeout_seconds = 6 * var.ingest_timeout_seconds
  message_retention_seconds  = 1209600
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.ingest_dlq.arn
    maxReceiveCount     = var.max_receive_count
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "ingest_dlq" {
  queue_url = aws_sqs_queue.ingest_dlq.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.ingest.arn]
  })
}

# Retains notifications SNS could not deliver to the ingestion queue at all,
# kept apart from the ingestion DLQ because the message formats differ.
resource "aws_sqs_queue" "delivery_dlq" {
  name                      = "${var.name}-receipt-delivery-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
}

# SNS may send to these queues only from the receipt topic.
resource "aws_sqs_queue_policy" "ingest" {
  queue_url = aws_sqs_queue.ingest.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "ReceiptTopicOnly"
      Effect    = "Allow"
      Principal = { Service = "sns.amazonaws.com" }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.ingest.arn
      Condition = { ArnEquals = { "aws:SourceArn" = aws_sns_topic.receipts.arn } }
    }]
  })
}

resource "aws_sqs_queue_policy" "delivery_dlq" {
  queue_url = aws_sqs_queue.delivery_dlq.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "ReceiptTopicOnly"
      Effect    = "Allow"
      Principal = { Service = "sns.amazonaws.com" }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.delivery_dlq.arn
      Condition = { ArnEquals = { "aws:SourceArn" = aws_sns_topic.receipts.arn } }
    }]
  })
}

# Keeps the SNS envelope (not raw delivery) so ingestion handles the same
# recorded SES S3-action SNS envelopes the local harness replays.
resource "aws_sns_topic_subscription" "ingest" {
  topic_arn = aws_sns_topic.receipts.arn
  protocol  = "sqs"
  endpoint  = aws_sqs_queue.ingest.arn

  redrive_policy = jsonencode({ deadLetterTargetArn = aws_sqs_queue.delivery_dlq.arn })

  depends_on = [aws_sqs_queue_policy.ingest, aws_sqs_queue_policy.delivery_dlq]
}

# ---------------------------------------------------------------------------
# SES domain identity and receipt rules. One delivery rule followed by a stop
# action; recipients that match no rule are rejected during the SMTP
# transaction (docs/specs/mail-flow.md#alias-policy).

resource "aws_ses_domain_identity" "this" {
  domain = var.domain
}

resource "aws_ses_receipt_rule_set" "this" {
  rule_set_name = local.rule_set_name
}

resource "aws_ses_receipt_rule" "deliver" {
  name          = local.rule_name
  rule_set_name = aws_ses_receipt_rule_set.this.rule_set_name
  recipients    = local.recipients
  enabled       = true
  scan_enabled  = true
  tls_policy    = "Optional"

  s3_action {
    position          = 1
    bucket_name       = aws_s3_bucket.originals.id
    object_key_prefix = var.ses_key_prefix
    topic_arn         = aws_sns_topic.receipts.arn
  }

  stop_action {
    position = 2
    scope    = "RuleSet"
  }

  # SES checks that it can write to the bucket and publish to the topic when
  # the rule is created.
  depends_on = [aws_s3_bucket_policy.originals, aws_sns_topic_policy.receipts]
}

resource "aws_ses_active_receipt_rule_set" "this" {
  rule_set_name = aws_ses_receipt_rule_set.this.rule_set_name

  depends_on = [aws_ses_receipt_rule.deliver]
}

# ---------------------------------------------------------------------------
# Processing Lambda: consumes the ingestion queue and commits each delivery
# to the mail table. Its role reads originals but cannot write or delete
# them (docs/specs/security.md#iam-role-requirements).

resource "aws_cloudwatch_log_group" "ingest" {
  name              = "/aws/lambda/${var.name}-ingest"
  retention_in_days = var.log_retention_days
}

resource "aws_iam_role" "ingest" {
  name = "${var.name}-ingest"
  path = "/s3mail/"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "ingest" {
  name = "ingest"
  role = aws_iam_role.ingest.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "WriteOwnLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.ingest.arn}:*"
      },
      {
        Sid      = "ConsumeIngestQueue"
        Effect   = "Allow"
        Action   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"]
        Resource = aws_sqs_queue.ingest.arn
      },
      {
        Sid      = "ReadOriginalVersions"
        Effect   = "Allow"
        Action   = "s3:GetObjectVersion"
        Resource = "${aws_s3_bucket.originals.arn}/${var.ses_key_prefix}*"
      },
      {
        Sid       = "ListOriginalVersions"
        Effect    = "Allow"
        Action    = "s3:ListBucketVersions"
        Resource  = aws_s3_bucket.originals.arn
        Condition = { StringLike = { "s3:prefix" = "${var.ses_key_prefix}*" } }
      },
      {
        Sid      = "CommitToOwnAccountPartition"
        Effect   = "Allow"
        Action   = ["dynamodb:GetItem", "dynamodb:Query", "dynamodb:BatchGetItem", "dynamodb:PutItem", "dynamodb:UpdateItem"]
        Resource = var.table_arn
        Condition = {
          "ForAllValues:StringEquals" = { "dynamodb:LeadingKeys" = [var.account_partition] }
        }
      },
      {
        # X-Ray accepts no resource ARNs for these actions.
        Sid      = "WriteTraces"
        Effect   = "Allow"
        Action   = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"]
        Resource = "*"
      },
    ]
  })
}

resource "aws_lambda_function" "ingest" {
  function_name    = "${var.name}-ingest"
  role             = aws_iam_role.ingest.arn
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  handler          = "bootstrap"
  filename         = var.ingest_zip
  source_code_hash = filebase64sha256(var.ingest_zip)
  memory_size      = 256
  timeout          = var.ingest_timeout_seconds
  publish          = true

  environment {
    variables = {
      ACCOUNT_ID = var.account_id
      TABLE      = var.table_name
      BUCKET     = aws_s3_bucket.originals.id
      KEY_PREFIX = var.ses_key_prefix
      TOPIC_ARN  = aws_sns_topic.receipts.arn
      DOMAIN     = var.domain
      ALIASES    = join(",", local.local_parts)
    }
  }

  logging_config {
    log_format            = "JSON"
    log_group             = aws_cloudwatch_log_group.ingest.name
    application_log_level = "INFO"
    system_log_level      = "WARN"
  }

  tracing_config {
    mode = "Active"
  }

  depends_on = [aws_iam_role_policy.ingest]
}

resource "aws_lambda_alias" "ingest_live" {
  name             = "live"
  function_name    = aws_lambda_function.ingest.function_name
  function_version = aws_lambda_function.ingest.version
}

# One delivery per invocation: the visibility timeout covers one 60-second
# processing deadline, and failures are still reported per item.
resource "aws_lambda_event_source_mapping" "ingest" {
  event_source_arn        = aws_sqs_queue.ingest.arn
  function_name           = aws_lambda_alias.ingest_live.arn
  batch_size              = 1
  function_response_types = ["ReportBatchItemFailures"]

  scaling_config {
    maximum_concurrency = 2
  }
}
