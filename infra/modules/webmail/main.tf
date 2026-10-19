# Browser webmail (docs/specs/clients.md#browser-hosting-and-client-safety):
# the pinned upstream Next.js server as a Lambda container image, fronted by
# AWS Lambda Web Adapter, behind its own Regional REST API on the webmail
# custom domain. Mail is reached only through the user's OAuth session at the
# JMAP API, so the role has no mail, index or secret grants.

data "aws_region" "current" {}

data "aws_caller_identity" "current" {}

locals {
  region = data.aws_region.current.region
  fqdn   = "${var.subdomain}.${var.domain}"
}

# --- Function ----------------------------------------------------------------

resource "aws_cloudwatch_log_group" "this" {
  name              = "/aws/lambda/${var.name}"
  retention_in_days = var.log_retention_days
}

resource "aws_iam_role" "this" {
  name = var.name
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

# Logs and traces only (docs/operations/permissions.md#s3mail-webmail-lambda-execution-role).
resource "aws_iam_role_policy" "this" {
  name = "webmail"
  role = aws_iam_role.this.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "WriteOwnLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.this.arn}:*"
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

resource "aws_lambda_function" "this" {
  function_name                  = var.name
  role                           = aws_iam_role.this.arn
  package_type                   = "Image"
  image_uri                      = var.image_uri
  architectures                  = ["arm64"]
  memory_size                    = var.memory
  timeout                        = 30
  reserved_concurrent_executions = var.concurrency
  publish                        = true

  environment {
    variables = {
      # Lambda Web Adapter: forward to the Next.js server once its health
      # route answers, streaming responses to match the gateway integration.
      AWS_LWA_PORT                 = "3000"
      AWS_LWA_READINESS_CHECK_PATH = "/api/health"
      AWS_LWA_INVOKE_MODE          = "response_stream"

      # Upstream configuration (OAuth only: no Basic Auth and no
      # SESSION_SECRET, so no credentials are stored server-side).
      APP_NAME         = var.app_name
      JMAP_SERVER_URL  = var.jmap_server_url
      OAUTH_ENABLED    = "true"
      OAUTH_ONLY       = "true"
      OAUTH_CLIENT_ID  = var.oauth_client_id
      OAUTH_ISSUER_URL = var.oauth_issuer
      OAUTH_SCOPES     = join(" ", var.oauth_scopes)
      LOG_FORMAT       = "json"
      LOG_LEVEL        = var.log_level
    }
  }

  # The app writes its own leveled JSON lines; Lambda's JSON format would
  # filter them by a level field it does not recognize.
  logging_config {
    log_format = "Text"
    log_group  = aws_cloudwatch_log_group.this.name
  }

  tracing_config {
    mode = "Active"
  }

  depends_on = [aws_iam_role_policy.this]
}

resource "aws_lambda_alias" "live" {
  name             = "live"
  function_name    = aws_lambda_function.this.function_name
  function_version = aws_lambda_function.this.version
}

# --- REST API ----------------------------------------------------------------

# Every path, including login, the OAuth callback and static assets, goes
# to the server; its own session checks protect the application. There is
# no bearer authorizer here: the JMAP API keeps its own.
resource "aws_api_gateway_rest_api" "this" {
  name = var.name

  # Bodies of any type reach the adapter base64-encoded and intact.
  binary_media_types = ["*/*"]
  # Only the custom domain serves the webmail, so cookies and OAuth
  # callbacks have one origin.
  disable_execute_api_endpoint = true

  endpoint_configuration {
    types = ["REGIONAL"]
  }
}

resource "aws_api_gateway_resource" "proxy" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_rest_api.this.root_resource_id
  path_part   = "{proxy+}"
}

locals {
  routes = {
    root  = aws_api_gateway_rest_api.this.root_resource_id
    proxy = aws_api_gateway_resource.proxy.id
  }
}

resource "aws_api_gateway_method" "route" {
  for_each = local.routes

  rest_api_id   = aws_api_gateway_rest_api.this.id
  resource_id   = each.value
  http_method   = "ANY"
  authorization = "NONE"
}

resource "aws_api_gateway_integration" "route" {
  for_each = local.routes

  rest_api_id             = aws_api_gateway_rest_api.this.id
  resource_id             = each.value
  http_method             = aws_api_gateway_method.route[each.key].http_method
  type                    = "AWS_PROXY"
  integration_http_method = "POST"
  uri                     = "arn:aws:apigateway:${local.region}:lambda:path/2021-11-15/functions/${aws_lambda_alias.live.arn}/response-streaming-invocations"
  response_transfer_mode  = "STREAM"
  timeout_milliseconds    = 29000
}

resource "aws_lambda_permission" "api" {
  statement_id   = "AllowWebmailApi"
  action         = "lambda:InvokeFunction"
  function_name  = aws_lambda_function.this.function_name
  qualifier      = aws_lambda_alias.live.name
  principal      = "apigateway.amazonaws.com"
  source_arn     = "${aws_api_gateway_rest_api.this.execution_arn}/*/*"
  source_account = data.aws_caller_identity.current.account_id
}

resource "aws_api_gateway_deployment" "this" {
  rest_api_id = aws_api_gateway_rest_api.this.id

  triggers = {
    redeploy = sha1(jsonencode([
      aws_api_gateway_resource.proxy,
      aws_api_gateway_method.route,
      aws_api_gateway_integration.route,
      aws_api_gateway_rest_api.this.binary_media_types,
    ]))
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_api_gateway_stage" "this" {
  rest_api_id          = aws_api_gateway_rest_api.this.id
  deployment_id        = aws_api_gateway_deployment.this.id
  stage_name           = "live"
  xray_tracing_enabled = true
}

# A first page load fetches many static chunks, so the burst allows more
# than the JMAP API's.
resource "aws_api_gateway_method_settings" "all" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  stage_name  = aws_api_gateway_stage.this.stage_name
  method_path = "*/*"

  settings {
    throttling_rate_limit  = var.throttle.rate
    throttling_burst_limit = var.throttle.burst
  }
}

# --- Custom domain -----------------------------------------------------------

# Records go straight into the mail root's hosted zone: this root is applied
# separately, and the dns module leaves records it does not declare alone.
# Both are portable CNAMEs (docs/operations/deployment.md#dns-and-region).
resource "aws_acm_certificate" "this" {
  domain_name       = local.fqdn
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_route53_record" "validation" {
  zone_id = var.zone_id
  name    = one(aws_acm_certificate.this.domain_validation_options).resource_record_name
  type    = one(aws_acm_certificate.this.domain_validation_options).resource_record_type
  ttl     = 300
  records = [one(aws_acm_certificate.this.domain_validation_options).resource_record_value]
}

resource "aws_acm_certificate_validation" "this" {
  certificate_arn         = aws_acm_certificate.this.arn
  validation_record_fqdns = [aws_route53_record.validation.fqdn]
}

resource "aws_api_gateway_domain_name" "this" {
  domain_name              = local.fqdn
  regional_certificate_arn = aws_acm_certificate_validation.this.certificate_arn
  security_policy          = "TLS_1_2"

  endpoint_configuration {
    types = ["REGIONAL"]
  }
}

resource "aws_api_gateway_base_path_mapping" "this" {
  api_id      = aws_api_gateway_rest_api.this.id
  stage_name  = aws_api_gateway_stage.this.stage_name
  domain_name = aws_api_gateway_domain_name.this.domain_name
}

resource "aws_route53_record" "webmail" {
  zone_id = var.zone_id
  name    = local.fqdn
  type    = "CNAME"
  ttl     = 300
  records = [aws_api_gateway_domain_name.this.regional_domain_name]
}
