# Outbound submission and outbox dispatch (docs/specs/mail-flow.md#outbound-email,
# docs/specs/storage-sync.md#idempotency-and-durable-work).
#
# The table stream, filtered to outbox inserts, triggers the dispatcher
# within seconds; a slower scheduled sweep drains whatever a failed trigger
# left and checks the index against the change log. The dispatcher hands
# submissions to the submission queue and index work to the index queue.
# The worker sends through the relay adapter; the one-minute recovery
# sweep settles expired leases and re-enqueues lost wake-ups. Sending is
# paused unless the relay parameter names the configured relay. Each
# function has its own least-privilege role (docs/operations/permissions.md).

locals {
  own_partition = { "ForAllValues:StringEquals" = { "dynamodb:LeadingKeys" = [var.account_partition] } }
  # Lambda timeout stays below the two-minute attempt lease, so a killed
  # worker's lease always outlives it.
  worker_timeout = 90

  # Guarded commits write STATE, CHANGE, SUBMISSION, ATTEMPT, QUOTA and
  # OUTBOX records; nothing deletes.
  commit = {
    Sid       = "ReadAndCommitOwnAccountPartition"
    Effect    = "Allow"
    Action    = ["dynamodb:GetItem", "dynamodb:Query", "dynamodb:BatchGetItem", "dynamodb:PutItem", "dynamodb:UpdateItem"]
    Resource  = var.table_arn
    Condition = local.own_partition
  }
  read_relay_parameter = {
    Sid      = "ReadRelayParameter"
    Effect   = "Allow"
    Action   = "ssm:GetParameter"
    Resource = aws_ssm_parameter.relay.arn
  }

  functions = {
    # Reads and acknowledges (deletes) outbox entries: the only delete
    # grant, and outbox entries are its only purpose. It reads no mail
    # content and no relay secret.
    dispatcher = {
      zip         = var.dispatcher_zip
      memory      = 128
      timeout     = 60
      concurrency = null
      env         = { INDEX_QUEUE_URL = var.index_queue_url, SUBMIT_QUEUE_URL = aws_sqs_queue.submit.url }
      statements = [
        {
          Sid       = "DrainOwnAccountOutbox"
          Effect    = "Allow"
          Action    = ["dynamodb:GetItem", "dynamodb:Query", "dynamodb:DeleteItem"]
          Resource  = var.table_arn
          Condition = local.own_partition
        },
        {
          Sid      = "ReadTableStream"
          Effect   = "Allow"
          Action   = ["dynamodb:DescribeStream", "dynamodb:GetRecords", "dynamodb:GetShardIterator", "dynamodb:ListStreams"]
          Resource = var.stream_arn
        },
        {
          Sid      = "SendWakeUps"
          Effect   = "Allow"
          Action   = "sqs:SendMessage"
          Resource = [var.index_queue_arn, aws_sqs_queue.submit.arn]
        },
      ]
    }
    # The only function that talks to the relay or reads its secret. The
    # originals grant is read-only by exact version, for composed and
    # imported messages.
    worker = {
      zip         = var.worker_zip
      memory      = 256
      timeout     = local.worker_timeout
      concurrency = var.worker_concurrency
      env = {
        QUEUE_URL       = aws_sqs_queue.submit.url
        RELAY           = var.relay.name
        RELAY_PARAMETER = aws_ssm_parameter.relay.name
        SECRET_ID       = aws_secretsmanager_secret.relay.arn
        SMTP_HOST       = var.relay.smtp_host
        SMTP_PORT       = tostring(var.relay.smtp_port)
        HELO_NAME       = var.domain
      }
      statements = [
        local.commit,
        local.read_relay_parameter,
        {
          Sid      = "ReadRelaySecret"
          Effect   = "Allow"
          Action   = "secretsmanager:GetSecretValue"
          Resource = aws_secretsmanager_secret.relay.arn
        },
        {
          Sid      = "ReadSendMaterial"
          Effect   = "Allow"
          Action   = "s3:GetObjectVersion"
          Resource = ["${var.originals_bucket_arn}/composed/*", "${var.originals_bucket_arn}/imported/*"]
        },
        {
          Sid      = "ConsumeAndRequeue"
          Effect   = "Allow"
          Action   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes", "sqs:SendMessage"]
          Resource = aws_sqs_queue.submit.arn
        },
      ]
    }
    # Settles expired leases and re-enqueues due work; no relay secret and
    # no mail content.
    recovery = {
      zip         = var.recovery_zip
      memory      = 128
      timeout     = 60
      concurrency = 1
      env = {
        QUEUE_URL       = aws_sqs_queue.submit.url
        RELAY           = var.relay.name
        RELAY_PARAMETER = aws_ssm_parameter.relay.name
      }
      statements = [
        local.commit,
        local.read_relay_parameter,
        {
          Sid      = "SendWakeUps"
          Effect   = "Allow"
          Action   = "sqs:SendMessage"
          Resource = aws_sqs_queue.submit.arn
        },
      ]
    }
  }

  # Delivery reports (docs/specs/mail-flow.md#outbound-relay-interface).
  # The receiver checks the bearer secret and only queues; it has no table
  # or mail access. The delivery worker applies reports to submissions.
  webhook_functions = {
    webhook = {
      zip         = var.webhook_zip
      memory      = 128
      timeout     = 10
      concurrency = var.webhook_concurrency
      env         = { SECRET_ID = aws_secretsmanager_secret.webhook.arn, QUEUE_URL = aws_sqs_queue.delivery.url }
      statements = [
        {
          Sid      = "ReadWebhookSecret"
          Effect   = "Allow"
          Action   = "secretsmanager:GetSecretValue"
          Resource = aws_secretsmanager_secret.webhook.arn
        },
        {
          Sid      = "PersistNotifications"
          Effect   = "Allow"
          Action   = "sqs:SendMessage"
          Resource = aws_sqs_queue.delivery.arn
        },
      ]
    }
    delivery = {
      zip         = var.delivery_zip
      memory      = 128
      timeout     = 30
      concurrency = null
      env         = {}
      statements = [
        local.commit,
        {
          Sid      = "ConsumeNotifications"
          Effect   = "Allow"
          Action   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"]
          Resource = aws_sqs_queue.delivery.arn
        },
      ]
    }
  }

  all_functions = merge(local.functions, local.webhook_functions)

  assume_lambda = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

# --- Relay configuration -----------------------------------------------------

# The value is set out of band ({"username","password"} JSON); Terraform
# creates only the empty secret and never holds the credential.
resource "aws_secretsmanager_secret" "relay" {
  name                    = "${var.name}/relay/${var.relay.name}"
  description             = "SMTP credentials for the ${var.relay.name} relay, set out of band."
  recovery_window_in_days = 7
}

# Names the active relay; any other value pauses sending. Changing it
# needs no deployment, so Terraform ignores the live value.
resource "aws_ssm_parameter" "relay" {
  name        = "/${var.name}/relay"
  description = "Active relay (${var.relay.name}) or paused."
  type        = "String"
  value       = "paused"

  lifecycle {
    ignore_changes = [value]
  }
}

# --- Functions ---------------------------------------------------------------

resource "aws_cloudwatch_log_group" "fn" {
  for_each = local.all_functions

  name              = "/aws/lambda/${var.name}-outbound-${each.key}"
  retention_in_days = var.log_retention_days
}

resource "aws_iam_role" "fn" {
  for_each = local.all_functions

  name               = "${var.name}-outbound-${each.key}"
  path               = "/s3mail/"
  assume_role_policy = local.assume_lambda
}

resource "aws_iam_role_policy" "fn" {
  for_each = local.all_functions

  name = "outbound-${each.key}"
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
    )
  })
}

resource "aws_lambda_function" "fn" {
  for_each = local.all_functions

  function_name                  = "${var.name}-outbound-${each.key}"
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

  environment {
    variables = merge({ TABLE = var.table_name, ACCOUNT_ID = var.account_id }, each.value.env)
  }

  logging_config {
    log_format            = "JSON"
    log_group             = aws_cloudwatch_log_group.fn[each.key].name
    application_log_level = "INFO"
    system_log_level      = "WARN"
  }

  depends_on = [aws_iam_role_policy.fn]
}

resource "aws_lambda_alias" "live" {
  for_each = local.all_functions

  name             = "live"
  function_name    = aws_lambda_function.fn[each.key].function_name
  function_version = aws_lambda_function.fn[each.key].version
}

# --- Dispatch ----------------------------------------------------------------

# KEYS_ONLY records filtered to outbox inserts. A batch that keeps failing
# is dropped after a few retries: the entries stay in the table for the
# sweep, so nothing is lost.
resource "aws_lambda_event_source_mapping" "outbox" {
  event_source_arn                   = var.stream_arn
  function_name                      = aws_lambda_alias.live["dispatcher"].arn
  starting_position                  = "LATEST"
  batch_size                         = 100
  maximum_batching_window_in_seconds = 1
  maximum_retry_attempts             = 3
  maximum_record_age_in_seconds      = 3600

  filter_criteria {
    filter {
      pattern = jsonencode({
        eventName = ["INSERT"]
        dynamodb  = { Keys = { SK = { S = [{ prefix = "OUTBOX#" }] } } }
      })
    }
  }
}

resource "aws_cloudwatch_event_rule" "schedule" {
  for_each = {
    dispatcher = var.sweep_schedule
    recovery   = var.recovery_schedule
  }

  name                = "${var.name}-outbound-${each.key}"
  schedule_expression = each.value
}

resource "aws_cloudwatch_event_target" "schedule" {
  for_each = aws_cloudwatch_event_rule.schedule

  rule = each.value.name
  arn  = aws_lambda_alias.live[each.key].arn
}

resource "aws_lambda_permission" "schedule" {
  for_each = aws_cloudwatch_event_rule.schedule

  statement_id  = "AllowSchedule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.fn[each.key].function_name
  qualifier     = aws_lambda_alias.live[each.key].name
  principal     = "events.amazonaws.com"
  source_arn    = each.value.arn
}

# --- Submission queue --------------------------------------------------------

# Wake-ups carry only a submission id; the durable submission is the
# authority, so a dead-lettered wake-up loses nothing: recovery re-enqueues
# due work.
resource "aws_sqs_queue" "submit_dlq" {
  name                      = "${var.name}-submit-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
}

resource "aws_sqs_queue" "submit" {
  name                       = "${var.name}-submit"
  visibility_timeout_seconds = local.worker_timeout * 2
  message_retention_seconds  = 345600
  sqs_managed_sse_enabled    = true

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.submit_dlq.arn
    maxReceiveCount     = 5
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "submit_dlq" {
  queue_url = aws_sqs_queue.submit_dlq.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.submit.arn]
  })
}

resource "aws_lambda_event_source_mapping" "submit" {
  event_source_arn        = aws_sqs_queue.submit.arn
  function_name           = aws_lambda_alias.live["worker"].arn
  batch_size              = 1
  function_response_types = ["ReportBatchItemFailures"]
}

# --- Delivery reports --------------------------------------------------------

# The bearer secret SMTP2GO sends in its Authorization header. Terraform
# creates it empty; the value is generated and set out of band, and a
# rotation keeps the previous version accepted until the relay is updated.
resource "aws_secretsmanager_secret" "webhook" {
  name                    = "${var.name}/relay/${var.relay.name}-webhook"
  description             = "Bearer secret for ${var.relay.name} delivery webhooks, set out of band."
  recovery_window_in_days = 7
}

# Reports are persisted here before the webhook is acknowledged; ones
# that cannot be parsed or correlated end in the DLQ for inspection.
resource "aws_sqs_queue" "delivery_dlq" {
  name                      = "${var.name}-delivery-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
}

resource "aws_sqs_queue" "delivery" {
  name = "${var.name}-delivery"
  # Five receives five minutes apart give a report half an hour to find
  # its submission.
  visibility_timeout_seconds = 300
  message_retention_seconds  = 1209600
  sqs_managed_sse_enabled    = true

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.delivery_dlq.arn
    maxReceiveCount     = 5
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "delivery_dlq" {
  queue_url = aws_sqs_queue.delivery_dlq.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.delivery.arn]
  })
}

resource "aws_lambda_event_source_mapping" "delivery" {
  event_source_arn        = aws_sqs_queue.delivery.arn
  function_name           = aws_lambda_alias.live["delivery"].arn
  batch_size              = 10
  function_response_types = ["ReportBatchItemFailures"]
}
