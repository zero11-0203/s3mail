# A1 event trigger (docs/specs/agent.md#architecture): the agent's rule on
# the mail system's EmailReceived events (ids only), a queue with a DLQ and
# the trigger Lambda, which starts one triage run per event on the live
# endpoint, plus the optional daily digest schedule. The rule is the
# agent's: deleting this root leaves the bus dropping events, so mail never
# depends on the agent (docs/specs/mail-flow.md#inbound-email).

resource "aws_cloudwatch_event_rule" "received" {
  count = local.trigger ? 1 : 0

  name           = "${var.resource_prefix}-received"
  event_bus_name = var.event_bus_name
  event_pattern = jsonencode({
    source        = ["s3mail.mail"]
    "detail-type" = ["EmailReceived"]
  })
}

resource "aws_cloudwatch_event_target" "received" {
  count = local.trigger ? 1 : 0

  rule           = aws_cloudwatch_event_rule.received[0].name
  event_bus_name = var.event_bus_name
  arn            = aws_sqs_queue.triage[0].arn
}

# A run that keeps failing (an expired login, the budget, JMAP errors) ends
# in the DLQ, which the owner redrives after fixing the cause; triage rows
# make a redrive harmless.
resource "aws_sqs_queue" "triage_dlq" {
  count = local.trigger ? 1 : 0

  name                      = "${var.resource_prefix}-triage-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
}

resource "aws_sqs_queue" "triage" {
  count = local.trigger ? 1 : 0

  name                       = "${var.resource_prefix}-triage"
  visibility_timeout_seconds = var.trigger_timeout_seconds * 6
  message_retention_seconds  = 345600
  sqs_managed_sse_enabled    = true

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.triage_dlq[0].arn
    maxReceiveCount     = 5
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "triage_dlq" {
  count = local.trigger ? 1 : 0

  queue_url = aws_sqs_queue.triage_dlq[0].id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.triage[0].arn]
  })
}

# Only the agent's rule may send.
resource "aws_sqs_queue_policy" "triage" {
  count = local.trigger ? 1 : 0

  queue_url = aws_sqs_queue.triage[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "FromReceivedRule"
      Effect    = "Allow"
      Principal = { Service = "events.amazonaws.com" }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.triage[0].arn
      Condition = { ArnEquals = { "aws:SourceArn" = aws_cloudwatch_event_rule.received[0].arn } }
    }]
  })
}

resource "aws_cloudwatch_log_group" "trigger" {
  count = local.trigger ? 1 : 0

  name              = "/aws/lambda/${var.resource_prefix}-trigger"
  retention_in_days = var.log_retention_days
}

resource "aws_iam_role" "trigger" {
  count = local.trigger ? 1 : 0

  name = "${var.resource_prefix}-trigger"
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

# docs/operations/permissions.md#s3mail-agent-trigger-agent-trigger-lambda
resource "aws_iam_role_policy" "trigger" {
  count = local.trigger ? 1 : 0

  name = "agent-trigger"
  role = aws_iam_role.trigger[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "WriteOwnLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.trigger[0].arn}:*"
      },
      {
        Sid      = "ConsumeQueue"
        Effect   = "Allow"
        Action   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"]
        Resource = aws_sqs_queue.triage[0].arn
      },
      {
        # The owner's user id is sent, so the ForUser action is needed too.
        Sid    = "InvokeLive"
        Effect = "Allow"
        Action = ["bedrock-agentcore:InvokeAgentRuntime", "bedrock-agentcore:InvokeAgentRuntimeForUser"]
        Resource = [
          aws_bedrockagentcore_agent_runtime.this[0].agent_runtime_arn,
          "${aws_bedrockagentcore_agent_runtime.this[0].agent_runtime_arn}/runtime-endpoint/live",
        ]
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

resource "aws_lambda_function" "trigger" {
  count = local.trigger ? 1 : 0

  function_name    = "${var.resource_prefix}-trigger"
  role             = aws_iam_role.trigger[0].arn
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  handler          = "bootstrap"
  filename         = var.trigger_zip
  source_code_hash = filebase64sha256(var.trigger_zip)
  memory_size      = 128
  timeout          = var.trigger_timeout_seconds

  environment {
    variables = {
      RUNTIME_ARN = aws_bedrockagentcore_agent_runtime.this[0].agent_runtime_arn
      QUALIFIER   = aws_bedrockagentcore_agent_runtime_endpoint.live[0].name
    }
  }

  logging_config {
    log_format            = "JSON"
    log_group             = aws_cloudwatch_log_group.trigger[0].name
    application_log_level = "INFO"
    system_log_level      = "WARN"
  }

  tracing_config {
    mode = "Active"
  }

  depends_on = [aws_iam_role_policy.trigger]
}

# One message per run and at most trigger_concurrency runs at once, which
# bounds model spend during a burst of mail.
resource "aws_lambda_event_source_mapping" "triage" {
  count = local.trigger ? 1 : 0

  event_source_arn        = aws_sqs_queue.triage[0].arn
  function_name           = aws_lambda_function.trigger[0].arn
  batch_size              = 1
  function_response_types = ["ReportBatchItemFailures"]

  scaling_config {
    maximum_concurrency = var.trigger_concurrency
  }
}

# The daily digest, delivered into the Agent mailbox. It needs mail.write,
# so it stays unset until that scope is granted (docs/specs/agent.md#features).
resource "aws_cloudwatch_event_rule" "digest" {
  count = local.trigger && var.digest_schedule != null ? 1 : 0

  name                = "${var.resource_prefix}-digest"
  schedule_expression = var.digest_schedule
}

resource "aws_cloudwatch_event_target" "digest" {
  count = length(aws_cloudwatch_event_rule.digest)

  rule  = aws_cloudwatch_event_rule.digest[0].name
  arn   = aws_lambda_function.trigger[0].arn
  input = jsonencode({ digest = "mail" })
}

resource "aws_lambda_permission" "digest" {
  count = length(aws_cloudwatch_event_rule.digest)

  statement_id  = "AllowDigestSchedule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.trigger[0].function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.digest[0].arn
}
