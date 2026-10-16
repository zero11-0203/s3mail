# Phase 0 response-streaming proof (docs/roadmap.md): a Go Lambda behind a
# Regional REST API whose integrations use STREAM mode. Temporary; destroy
# it after recording results. Methods use AWS_IAM authorization so only the
# owner's signed requests reach it; the Cognito authorizer is a later slice.

data "aws_region" "current" {}

data "aws_caller_identity" "current" {}

# --- Lambda --------------------------------------------------------------

resource "aws_cloudwatch_log_group" "lambda" {
  name              = "/aws/lambda/${var.name}"
  retention_in_days = 7
}

data "aws_iam_policy_document" "assume_lambda" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "lambda" {
  name               = var.name
  path               = "/s3mail/"
  assume_role_policy = data.aws_iam_policy_document.assume_lambda.json
}

# Least privilege: write only to this function's pre-created log group.
data "aws_iam_policy_document" "lambda" {
  statement {
    sid       = "WriteOwnLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.lambda.arn}:*"]
  }
}

resource "aws_iam_role_policy" "lambda" {
  name   = "logs"
  role   = aws_iam_role.lambda.id
  policy = data.aws_iam_policy_document.lambda.json
}

resource "aws_lambda_function" "this" {
  function_name    = var.name
  role             = aws_iam_role.lambda.arn
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  handler          = "bootstrap"
  filename         = var.lambda_zip
  source_code_hash = filebase64sha256(var.lambda_zip)
  memory_size      = 256
  timeout          = 900
  publish          = true

  depends_on = [aws_cloudwatch_log_group.lambda, aws_iam_role_policy.lambda]
}

resource "aws_lambda_alias" "live" {
  name             = "live"
  function_name    = aws_lambda_function.this.function_name
  function_version = aws_lambda_function.this.version
}

# --- REST API --------------------------------------------------------------

resource "aws_api_gateway_rest_api" "this" {
  name = var.name

  # Binary request bodies reach Lambda base64-encoded.
  binary_media_types = ["application/octet-stream"]

  endpoint_configuration {
    types = ["REGIONAL"]
  }
}

locals {
  routes = {
    stream   = "GET"
    download = "GET"
    upload   = "POST"
  }

  # InvokeWithResponseStream on the live alias, required for STREAM mode.
  streaming_uri = "arn:aws:apigateway:${data.aws_region.current.region}:lambda:path/2021-11-15/functions/${aws_lambda_alias.live.arn}/response-streaming-invocations"
}

resource "aws_api_gateway_resource" "route" {
  for_each = local.routes

  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_rest_api.this.root_resource_id
  path_part   = each.key
}

resource "aws_api_gateway_method" "route" {
  for_each = local.routes

  rest_api_id   = aws_api_gateway_rest_api.this.id
  resource_id   = aws_api_gateway_resource.route[each.key].id
  http_method   = each.value
  authorization = "AWS_IAM"
}

resource "aws_api_gateway_integration" "route" {
  for_each = local.routes

  rest_api_id             = aws_api_gateway_rest_api.this.id
  resource_id             = aws_api_gateway_resource.route[each.key].id
  http_method             = aws_api_gateway_method.route[each.key].http_method
  type                    = "AWS_PROXY"
  integration_http_method = "POST"
  uri                     = local.streaming_uri
  response_transfer_mode  = "STREAM"
}

resource "aws_lambda_permission" "apigateway" {
  statement_id   = "AllowSpikeApi"
  action         = "lambda:InvokeFunction"
  function_name  = aws_lambda_function.this.function_name
  qualifier      = aws_lambda_alias.live.name
  principal      = "apigateway.amazonaws.com"
  source_arn     = "${aws_api_gateway_rest_api.this.execution_arn}/*/*/*"
  source_account = data.aws_caller_identity.current.account_id
}

resource "aws_api_gateway_deployment" "this" {
  rest_api_id = aws_api_gateway_rest_api.this.id

  triggers = {
    redeploy = sha1(jsonencode([
      aws_api_gateway_resource.route,
      aws_api_gateway_method.route,
      aws_api_gateway_integration.route,
      aws_api_gateway_rest_api.this.binary_media_types,
    ]))
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_api_gateway_stage" "spike" {
  rest_api_id   = aws_api_gateway_rest_api.this.id
  deployment_id = aws_api_gateway_deployment.this.id
  stage_name    = "spike"
}

# Initial interactive limits from docs/specs/security.md.
resource "aws_api_gateway_method_settings" "all" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  stage_name  = aws_api_gateway_stage.spike.stage_name
  method_path = "*/*"

  settings {
    throttling_rate_limit  = 5
    throttling_burst_limit = 10
  }
}
