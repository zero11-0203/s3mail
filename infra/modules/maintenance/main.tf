# Daily derived-data collection (docs/operations/recovery.md#retention-deletion-and-garbage-collection).
#
# The collector prunes change history behind the history floor and purges
# search index versions that may still hold destroyed Emails. Its role can
# delete items in the account partition (DynamoDB grants cannot narrow to
# CHANGE records) and index object versions; it has no grant on the
# original-mail bucket, so it can neither read nor delete original mail.

locals {
  index_arn = "arn:aws:s3:::${var.index_bucket}"
}

resource "aws_cloudwatch_log_group" "gc" {
  name              = "/aws/lambda/${var.name}-gc"
  retention_in_days = var.log_retention_days
}

resource "aws_iam_role" "gc" {
  name = "${var.name}-gc"
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

resource "aws_iam_role_policy" "gc" {
  name = "gc"
  role = aws_iam_role.gc.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "WriteOwnLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.gc.arn}:*"
      },
      {
        # Reads STATE, SEARCH and CHANGE records, moves the history floor
        # on STATE and deletes CHANGE records under it.
        Sid       = "PruneOwnAccountHistory"
        Effect    = "Allow"
        Action    = ["dynamodb:GetItem", "dynamodb:Query", "dynamodb:UpdateItem", "dynamodb:DeleteItem"]
        Resource  = var.table_arn
        Condition = { "ForAllValues:StringEquals" = { "dynamodb:LeadingKeys" = [var.account_partition] } }
      },
      {
        Sid       = "ListIndexVersions"
        Effect    = "Allow"
        Action    = "s3:ListBucketVersions"
        Resource  = local.index_arn
        Condition = { StringLike = { "s3:prefix" = ["index/*"] } }
      },
      {
        Sid      = "PurgeIndexVersions"
        Effect   = "Allow"
        Action   = "s3:DeleteObjectVersion"
        Resource = "${local.index_arn}/index/*"
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

resource "aws_lambda_function" "gc" {
  function_name                  = "${var.name}-gc"
  role                           = aws_iam_role.gc.arn
  runtime                        = "provided.al2023"
  architectures                  = ["arm64"]
  handler                        = "bootstrap"
  filename                       = var.gc_zip
  source_code_hash               = filebase64sha256(var.gc_zip)
  memory_size                    = 128
  timeout                        = 300
  reserved_concurrent_executions = 1
  publish                        = true

  environment {
    variables = { TABLE = var.table_name, ACCOUNT_ID = var.account_id, INDEX_BUCKET = var.index_bucket }
  }

  logging_config {
    log_format            = "JSON"
    log_group             = aws_cloudwatch_log_group.gc.name
    application_log_level = "INFO"
    system_log_level      = "WARN"
  }

  tracing_config {
    mode = "Active"
  }

  depends_on = [aws_iam_role_policy.gc]
}

resource "aws_lambda_alias" "live" {
  name             = "live"
  function_name    = aws_lambda_function.gc.function_name
  function_version = aws_lambda_function.gc.version
}

resource "aws_cloudwatch_event_rule" "gc" {
  name                = "${var.name}-gc"
  schedule_expression = var.schedule
  state               = var.enabled ? "ENABLED" : "DISABLED"
}

resource "aws_cloudwatch_event_target" "gc" {
  rule = aws_cloudwatch_event_rule.gc.name
  arn  = aws_lambda_alias.live.arn
}

resource "aws_lambda_permission" "gc" {
  statement_id  = "AllowSchedule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.gc.function_name
  qualifier     = aws_lambda_alias.live.name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.gc.arn
}
