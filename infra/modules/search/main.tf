# Text search (docs/specs/search.md#index-pipeline): the outbox dispatcher
# (mail-outbound) wakes a single-writer indexer through SQS; the indexer publishes SQLite
# snapshots to a versioned index bucket and registers each generation in
# the mail table; the search function serves the JMAP API from the
# registered generation. Each function has its own least-privilege role
# (docs/operations/permissions.md).

locals {
  own_partition = { "ForAllValues:StringEquals" = { "dynamodb:LeadingKeys" = [var.account_partition] } }
  index_objects = "${aws_s3_bucket.index.arn}/index/*"
  index_timeout = 900

  functions = {
    # The only index writer. A registration is a guarded commit: it
    # updates STATE and writes SEARCH and one CHANGE record; it never
    # deletes. The originals grant is read-only by exact version.
    indexer = {
      zip         = var.indexer_zip
      memory      = 2048
      timeout     = local.index_timeout
      storage     = 4096
      concurrency = 1
      env         = { INDEX_BUCKET = aws_s3_bucket.index.id }
      statements = [
        {
          Sid       = "ReadAndRegisterOwnAccountPartition"
          Effect    = "Allow"
          Action    = ["dynamodb:GetItem", "dynamodb:Query", "dynamodb:BatchGetItem", "dynamodb:PutItem", "dynamodb:UpdateItem"]
          Resource  = var.table_arn
          Condition = local.own_partition
        },
        {
          Sid      = "ReadOriginalVersions"
          Effect   = "Allow"
          Action   = "s3:GetObjectVersion"
          Resource = "${var.originals_bucket_arn}/*"
        },
        {
          Sid      = "PublishIndex"
          Effect   = "Allow"
          Action   = ["s3:GetObject", "s3:GetObjectVersion", "s3:PutObject"]
          Resource = local.index_objects
        },
        {
          # HeadObject on a missing key returns 404 rather than 403 only
          # with ListBucket; the bucket holds nothing but index objects.
          Sid      = "FindIndex"
          Effect   = "Allow"
          Action   = "s3:ListBucket"
          Resource = aws_s3_bucket.index.arn
        },
        {
          Sid      = "ConsumeWakeUps"
          Effect   = "Allow"
          Action   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"]
          Resource = aws_sqs_queue.index.arn
        },
      ]
    }
    # Reads the registration and exact registered index versions; it has
    # no mail-content or write grant.
    search = {
      zip         = var.search_zip
      memory      = 1024
      timeout     = 30
      storage     = 2048
      concurrency = null
      env         = { INDEX_BUCKET = aws_s3_bucket.index.id }
      statements = [
        {
          Sid       = "ReadOwnAccountPartition"
          Effect    = "Allow"
          Action    = ["dynamodb:GetItem", "dynamodb:Query"]
          Resource  = var.table_arn
          Condition = local.own_partition
        },
        {
          Sid      = "ReadIndexVersions"
          Effect   = "Allow"
          Action   = "s3:GetObjectVersion"
          Resource = local.index_objects
        },
      ]
    }
  }

  assume_lambda = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

# --- Functions ---------------------------------------------------------------

resource "aws_cloudwatch_log_group" "fn" {
  for_each = local.functions

  name              = "/aws/lambda/${var.name}-search-${each.key}"
  retention_in_days = var.log_retention_days
}

resource "aws_iam_role" "fn" {
  for_each = local.functions

  name               = "${var.name}-search-${each.key}"
  path               = "/s3mail/"
  assume_role_policy = local.assume_lambda
}

resource "aws_iam_role_policy" "fn" {
  for_each = local.functions

  name = "search-${each.key}"
  role = aws_iam_role.fn[each.key].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat(
      [{
        Sid      = "WriteOwnLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.fn[each.key].arn}:*"
      }],
      each.value.statements,
      [{
        # X-Ray accepts no resource ARNs for these actions.
        Sid      = "WriteTraces"
        Effect   = "Allow"
        Action   = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"]
        Resource = "*"
      }],
    )
  })
}

resource "aws_lambda_function" "fn" {
  for_each = local.functions

  function_name                  = "${var.name}-search-${each.key}"
  role                           = aws_iam_role.fn[each.key].arn
  runtime                        = "provided.al2023"
  architectures                  = ["arm64"]
  handler                        = "bootstrap"
  filename                       = each.value.zip
  source_code_hash               = filebase64sha256(each.value.zip)
  memory_size                    = each.value.memory
  timeout                        = each.value.timeout
  reserved_concurrent_executions = each.value.concurrency
  publish                        = true

  # Working copy, snapshot and old/new reader copies live in /tmp.
  ephemeral_storage {
    size = each.value.storage
  }

  environment {
    variables = merge({ TABLE = var.table_name, ACCOUNT_ID = var.account_id }, each.value.env)
  }

  logging_config {
    log_format            = "JSON"
    log_group             = aws_cloudwatch_log_group.fn[each.key].name
    application_log_level = "INFO"
    system_log_level      = "WARN"
  }

  tracing_config {
    mode = "Active"
  }

  depends_on = [aws_iam_role_policy.fn]
}

resource "aws_lambda_alias" "live" {
  for_each = local.functions

  name             = "live"
  function_name    = aws_lambda_function.fn[each.key].function_name
  function_version = aws_lambda_function.fn[each.key].version
}

# --- Wake-ups ----------------------------------------------------------------

# Wake-ups carry no source progress, so a dead-lettered one loses nothing:
# the next dispatch sweep wakes the indexer again.
resource "aws_sqs_queue" "index_dlq" {
  name                      = "${var.name}-index-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
}

resource "aws_sqs_queue" "index" {
  name = "${var.name}-index"
  # Throttled batches (reserved concurrency one) stay invisible this long,
  # so it is kept just above the function timeout plus batching window.
  visibility_timeout_seconds = local.index_timeout + 120
  message_retention_seconds  = 86400
  sqs_managed_sse_enabled    = true

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.index_dlq.arn
    maxReceiveCount     = 5
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "index_dlq" {
  queue_url = aws_sqs_queue.index_dlq.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.index.arn]
  })
}

# Wake-ups are coalesced for a minute; the stream-triggered dispatcher
# sends them within seconds of a commit, so a new Email is searchable well
# within the six-minute target. Reserved concurrency one serializes runs
# (the mapping's own cap cannot go below two); a throttled batch is lost
# at worst, and the dispatcher's sweep wakes again.
resource "aws_lambda_event_source_mapping" "index" {
  event_source_arn                   = aws_sqs_queue.index.arn
  function_name                      = aws_lambda_alias.live["indexer"].arn
  batch_size                         = 100
  maximum_batching_window_in_seconds = 60
}

# --- Index bucket ------------------------------------------------------------

# Rebuildable derived data. Versioning gives readers exact generations;
# old versions are short rollback aids that expire after a day, keeping
# the three newest (docs/specs/search.md#why-sqlite-on-s3).
resource "aws_s3_bucket" "index" {
  bucket_prefix = "${var.name}-index-"
  force_destroy = true
}

resource "aws_s3_bucket_versioning" "index" {
  bucket = aws_s3_bucket.index.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_ownership_controls" "index" {
  bucket = aws_s3_bucket.index.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "index" {
  bucket                  = aws_s3_bucket.index.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "index" {
  bucket = aws_s3_bucket.index.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "index" {
  bucket = aws_s3_bucket.index.id

  rule {
    id     = "expire-old-generations"
    status = "Enabled"

    filter {
      prefix = "index/"
    }

    noncurrent_version_expiration {
      noncurrent_days           = 1
      newer_noncurrent_versions = 3
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }

  depends_on = [aws_s3_bucket_versioning.index]
}

resource "aws_s3_bucket_policy" "index" {
  bucket = aws_s3_bucket.index.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "TlsOnly"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [aws_s3_bucket.index.arn, "${aws_s3_bucket.index.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })

  depends_on = [aws_s3_bucket_public_access_block.index]
}
