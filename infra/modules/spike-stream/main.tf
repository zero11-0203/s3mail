# Phase 0 response-streaming proof (docs/roadmap.md): a Go Lambda behind a
# Regional REST API whose integrations use STREAM mode, protected by the
# shared bearer-token authorizer (docs/specs/security.md#shared-endpoint-authorization).
# Temporary; destroy it after recording results.

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

# --- Authorizer Lambda ----------------------------------------------------

resource "aws_cloudwatch_log_group" "authorizer" {
  name              = "/aws/lambda/${var.name}-authorizer"
  retention_in_days = 7
}

resource "aws_iam_role" "authorizer" {
  name               = "${var.name}-authorizer"
  path               = "/s3mail/"
  assume_role_policy = data.aws_iam_policy_document.assume_lambda.json
}

# Least privilege: the authorizer only fetches the public JWKS over HTTPS
# and writes its own logs.
data "aws_iam_policy_document" "authorizer" {
  statement {
    sid       = "WriteOwnLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.authorizer.arn}:*"]
  }
}

resource "aws_iam_role_policy" "authorizer" {
  name   = "logs"
  role   = aws_iam_role.authorizer.id
  policy = data.aws_iam_policy_document.authorizer.json
}

resource "aws_lambda_function" "authorizer" {
  function_name    = "${var.name}-authorizer"
  role             = aws_iam_role.authorizer.arn
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  handler          = "bootstrap"
  filename         = var.authorizer_zip
  source_code_hash = filebase64sha256(var.authorizer_zip)
  memory_size      = 128
  timeout          = 10

  environment {
    variables = {
      ISSUER     = var.issuer
      CLIENT_IDS = join(",", var.client_ids)
    }
  }

  depends_on = [aws_cloudwatch_log_group.authorizer, aws_iam_role_policy.authorizer]
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
  stage_name = "spike"
  # The protected resource identifier and its RFC 9728 metadata URL. The
  # stage path prefix is spike-only; the real API uses a custom domain.
  resource_url = "https://${aws_api_gateway_rest_api.this.id}.execute-api.${data.aws_region.current.region}.amazonaws.com/${local.stage_name}"
  metadata_url = "${local.resource_url}/.well-known/oauth-protected-resource"

  routes = {
    stream   = "GET"
    download = "GET"
    upload   = "POST"
  }

  # InvokeWithResponseStream on the live alias, required for STREAM mode.
  streaming_uri = "arn:aws:apigateway:${data.aws_region.current.region}:lambda:path/2021-11-15/functions/${aws_lambda_alias.live.arn}/response-streaming-invocations"
}

# Every method uses the shared bearer-token authorizer. Result caching is
# off (TTL 0), so every request is verified. The validation expression lets
# the gateway reject a missing or non-Bearer header without invoking it.
resource "aws_api_gateway_authorizer" "bearer" {
  name                             = "bearer"
  rest_api_id                      = aws_api_gateway_rest_api.this.id
  type                             = "TOKEN"
  authorizer_uri                   = aws_lambda_function.authorizer.invoke_arn
  identity_source                  = "method.request.header.Authorization"
  identity_validation_expression   = "^Bearer [-0-9A-Za-z._~+/]+=*$"
  authorizer_result_ttl_in_seconds = 0
}

resource "aws_lambda_permission" "authorizer" {
  statement_id   = "AllowSpikeApiAuthorizer"
  action         = "lambda:InvokeFunction"
  function_name  = aws_lambda_function.authorizer.function_name
  principal      = "apigateway.amazonaws.com"
  source_arn     = "${aws_api_gateway_rest_api.this.execution_arn}/authorizers/${aws_api_gateway_authorizer.bearer.id}"
  source_account = data.aws_caller_identity.current.account_id
}

# 401s carry a Bearer challenge naming the protected resource metadata, so
# OAuth clients can discover the authorization server (RFC 6750, RFC 9728).
resource "aws_api_gateway_gateway_response" "unauthorized" {
  rest_api_id   = aws_api_gateway_rest_api.this.id
  response_type = "UNAUTHORIZED"
  status_code   = "401"

  response_parameters = {
    "gatewayresponse.header.WWW-Authenticate" = "'Bearer resource_metadata=\"${local.metadata_url}\"'"
    "gatewayresponse.header.Cache-Control"    = "'no-store'"
  }
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
  authorization = "CUSTOM"
  authorizer_id = aws_api_gateway_authorizer.bearer.id
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

  # The 29 s default still caps total response time in STREAM mode
  # (measured: streams cut at ~30 s); raise it to the 15-minute maximum.
  timeout_milliseconds = 900000
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

# --- Protected resource metadata (RFC 9728), public -----------------------

resource "aws_api_gateway_resource" "well_known" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_rest_api.this.root_resource_id
  path_part   = ".well-known"
}

resource "aws_api_gateway_resource" "metadata" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_resource.well_known.id
  path_part   = "oauth-protected-resource"
}

resource "aws_api_gateway_method" "metadata" {
  rest_api_id   = aws_api_gateway_rest_api.this.id
  resource_id   = aws_api_gateway_resource.metadata.id
  http_method   = "GET"
  authorization = "NONE"
}

# A static MOCK response: no Lambda and no data behind it.
resource "aws_api_gateway_integration" "metadata" {
  rest_api_id       = aws_api_gateway_rest_api.this.id
  resource_id       = aws_api_gateway_resource.metadata.id
  http_method       = aws_api_gateway_method.metadata.http_method
  type              = "MOCK"
  request_templates = { "application/json" = "{\"statusCode\": 200}" }
}

resource "aws_api_gateway_method_response" "metadata" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  resource_id = aws_api_gateway_resource.metadata.id
  http_method = aws_api_gateway_method.metadata.http_method
  status_code = "200"

  response_parameters = {
    "method.response.header.Cache-Control" = true
  }
}

resource "aws_api_gateway_integration_response" "metadata" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  resource_id = aws_api_gateway_resource.metadata.id
  http_method = aws_api_gateway_method.metadata.http_method
  status_code = aws_api_gateway_method_response.metadata.status_code

  response_parameters = {
    "method.response.header.Cache-Control" = "'max-age=300'"
  }
  response_templates = {
    "application/json" = jsonencode({
      resource                 = local.resource_url
      authorization_servers    = [var.issuer]
      scopes_supported         = var.scopes
      bearer_methods_supported = ["header"]
    })
  }

  depends_on = [aws_api_gateway_integration.metadata]
}

resource "aws_api_gateway_deployment" "this" {
  rest_api_id = aws_api_gateway_rest_api.this.id

  triggers = {
    redeploy = sha1(jsonencode([
      aws_api_gateway_resource.route,
      aws_api_gateway_method.route,
      aws_api_gateway_integration.route,
      aws_api_gateway_rest_api.this.binary_media_types,
      aws_api_gateway_authorizer.bearer,
      aws_api_gateway_gateway_response.unauthorized,
      aws_api_gateway_method.metadata,
      aws_api_gateway_integration.metadata,
      aws_api_gateway_integration_response.metadata,
    ]))
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_api_gateway_stage" "spike" {
  rest_api_id   = aws_api_gateway_rest_api.this.id
  deployment_id = aws_api_gateway_deployment.this.id
  stage_name    = local.stage_name
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
