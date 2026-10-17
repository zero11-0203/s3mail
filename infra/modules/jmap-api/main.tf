# The JMAP API (docs/specs/jmap.md): a Regional REST API whose integrations
# stream from Go Lambdas, behind the shared bearer-token authorizer
# (docs/specs/security.md#shared-endpoint-authorization). Session/API,
# download and push run as separate functions, each with its own
# least-privilege role (docs/operations/permissions.md).

data "aws_region" "current" {}

data "aws_caller_identity" "current" {}

locals {
  region = data.aws_region.current.region

  # Until a custom domain is mapped, the stage is part of every URL.
  base_url     = "https://${aws_api_gateway_rest_api.this.id}.execute-api.${local.region}.amazonaws.com/${var.stage_name}"
  metadata_url = "${local.base_url}/.well-known/oauth-protected-resource"

  read_originals = {
    Sid      = "ReadOriginalVersions"
    Effect   = "Allow"
    Action   = "s3:GetObjectVersion"
    Resource = "${var.originals_bucket_arn}/${var.ses_key_prefix}*"
  }
  own_partition = { "ForAllValues:StringEquals" = { "dynamodb:LeadingKeys" = [var.account_partition] } }

  # Imported and composed Emails read back their archived MIME.
  read_new_originals = {
    Sid      = "ReadNewOriginalVersions"
    Effect   = "Allow"
    Action   = "s3:GetObjectVersion"
    Resource = ["${var.originals_bucket_arn}/imported/*", "${var.originals_bucket_arn}/composed/*"]
  }
  read_uploads = {
    Sid      = "ReadUploads"
    Effect   = "Allow"
    Action   = "s3:GetObject"
    Resource = "${aws_s3_bucket.blobs.arn}/uploads/*"
  }

  functions = {
    # Session discovery, the JMAP method endpoint and uploads. Commits
    # write the account's records in one guarded transaction.
    api = {
      zip         = var.api_zip
      memory      = 1024
      timeout     = 30
      concurrency = null
      statements = concat([
        {
          Sid       = "ReadAndUpdateOwnAccountPartition"
          Effect    = "Allow"
          Action    = ["dynamodb:GetItem", "dynamodb:Query", "dynamodb:BatchGetItem", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem"]
          Resource  = var.table_arn
          Condition = local.own_partition
        },
        local.read_originals,
        local.read_new_originals,
        local.read_uploads,
        {
          # Durable MIME for Email/import and Email/set create. Keys are
          # new ids written with If-None-Match; no delete grant exists.
          Sid      = "ArchiveNewOriginals"
          Effect   = "Allow"
          Action   = "s3:PutObject"
          Resource = ["${var.originals_bucket_arn}/imported/*", "${var.originals_bucket_arn}/composed/*"]
        },
        {
          Sid      = "WriteUploads"
          Effect   = "Allow"
          Action   = "s3:PutObject"
          Resource = "${aws_s3_bucket.blobs.arn}/uploads/*"
        },
        ], var.search_function_arn == null ? [] : [{
          # Text conditions and snippets (docs/specs/search.md#index-pipeline).
          Sid      = "InvokeSearch"
          Effect   = "Allow"
          Action   = "lambda:InvokeFunction"
          Resource = var.search_function_arn
      }])
    }
    # Blob downloads: read-only.
    download = {
      zip         = var.download_zip
      memory      = 1024
      timeout     = 60
      concurrency = null
      statements = [
        {
          Sid       = "ReadOwnAccountPartition"
          Effect    = "Allow"
          Action    = ["dynamodb:GetItem", "dynamodb:BatchGetItem"]
          Resource  = var.table_arn
          Condition = local.own_partition
        },
        local.read_originals,
        local.read_new_originals,
        local.read_uploads,
      ]
    }
    # EventSource: polls STATE only. Reserved concurrency caps open streams
    # and their duration cost (docs/specs/jmap.md#push).
    push = {
      zip         = var.push_zip
      memory      = 128
      timeout     = 900
      concurrency = var.push_concurrency
      statements = [
        {
          Sid       = "ReadOwnAccountState"
          Effect    = "Allow"
          Action    = "dynamodb:GetItem"
          Resource  = var.table_arn
          Condition = local.own_partition
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

# --- JMAP functions ----------------------------------------------------------

resource "aws_cloudwatch_log_group" "fn" {
  for_each = local.functions

  name              = "/aws/lambda/${var.name}-jmap-${each.key}"
  retention_in_days = var.log_retention_days
}

resource "aws_iam_role" "fn" {
  for_each = local.functions

  name               = "${var.name}-jmap-${each.key}"
  path               = "/s3mail/"
  assume_role_policy = local.assume_lambda
}

resource "aws_iam_role_policy" "fn" {
  for_each = local.functions

  name = "jmap-${each.key}"
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

  function_name                  = "${var.name}-jmap-${each.key}"
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
    variables = merge({
      TABLE            = var.table_name
      ACCOUNT_ID       = var.account_id
      OWNER_SUB        = var.owner_subject
      PUBLIC_URL       = local.base_url
      ORIGINALS_BUCKET = var.originals_bucket_name
      BLOBS_BUCKET     = aws_s3_bucket.blobs.id
    }, each.key == "api" && var.search_function_arn != null ? { SEARCH_FUNCTION = var.search_function_arn } : {})
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

# --- Disposable blob bucket ----------------------------------------------------

# Uploads only: unversioned, expiring after a day, separate from the
# permanent originals (docs/operations/recovery.md#retention-deletion-and-garbage-collection).
# An import copies the complete MIME into the original bucket first.
resource "aws_s3_bucket" "blobs" {
  bucket_prefix = "${var.name}-blobs-"
  force_destroy = true
}

resource "aws_s3_bucket_ownership_controls" "blobs" {
  bucket = aws_s3_bucket.blobs.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "blobs" {
  bucket                  = aws_s3_bucket.blobs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "blobs" {
  bucket = aws_s3_bucket.blobs.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "blobs" {
  bucket = aws_s3_bucket.blobs.id

  rule {
    id     = "expire-uploads"
    status = "Enabled"

    filter {
      prefix = "uploads/"
    }

    # Uploads stop resolving at 24 hours; S3 then removes the bytes at
    # the next midnight UTC after a day.
    expiration {
      days = 1
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
}

resource "aws_s3_bucket_policy" "blobs" {
  bucket = aws_s3_bucket.blobs.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "TlsOnly"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [aws_s3_bucket.blobs.arn, "${aws_s3_bucket.blobs.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })

  depends_on = [aws_s3_bucket_public_access_block.blobs]
}

# --- Authorizer --------------------------------------------------------------

resource "aws_cloudwatch_log_group" "authorizer" {
  name              = "/aws/lambda/${var.name}-authorizer"
  retention_in_days = var.log_retention_days
}

resource "aws_iam_role" "authorizer" {
  name               = "${var.name}-authorizer"
  path               = "/s3mail/"
  assume_role_policy = local.assume_lambda
}

# The authorizer only fetches the public JWKS over HTTPS and writes its
# own logs.
resource "aws_iam_role_policy" "authorizer" {
  name = "logs"
  role = aws_iam_role.authorizer.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "WriteOwnLogs"
      Effect   = "Allow"
      Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
      Resource = "${aws_cloudwatch_log_group.authorizer.arn}:*"
    }]
  })
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

  logging_config {
    log_format            = "JSON"
    log_group             = aws_cloudwatch_log_group.authorizer.name
    application_log_level = "INFO"
    system_log_level      = "WARN"
  }

  depends_on = [aws_iam_role_policy.authorizer]
}

# --- REST API ----------------------------------------------------------------

resource "aws_api_gateway_rest_api" "this" {
  name = "${var.name}-jmap"

  # Every request body reaches Lambda base64-encoded, so uploads of any
  # type keep their exact bytes; the handlers decode JSON bodies too.
  binary_media_types = ["*/*"]

  endpoint_configuration {
    types = ["REGIONAL"]
  }
}

# Result caching is off (TTL 0), so every request is verified. The
# validation expression lets the gateway reject a missing or non-Bearer
# header without invoking the authorizer.
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
  statement_id   = "AllowJmapApiAuthorizer"
  action         = "lambda:InvokeFunction"
  function_name  = aws_lambda_function.authorizer.function_name
  principal      = "apigateway.amazonaws.com"
  source_arn     = "${aws_api_gateway_rest_api.this.execution_arn}/authorizers/${aws_api_gateway_authorizer.bearer.id}"
  source_account = data.aws_caller_identity.current.account_id
}

# 401s carry a Bearer challenge naming the protected resource metadata
# (RFC 6750, RFC 9728).
resource "aws_api_gateway_gateway_response" "unauthorized" {
  rest_api_id   = aws_api_gateway_rest_api.this.id
  response_type = "UNAUTHORIZED"
  status_code   = "401"

  response_parameters = {
    "gatewayresponse.header.WWW-Authenticate" = "'Bearer resource_metadata=\"${local.metadata_url}\"'"
    "gatewayresponse.header.Cache-Control"    = "'no-store'"
  }
  # The default body, declared so plans stay clean.
  response_templates = {
    "application/json" = "{\"message\":$context.error.messageString}"
  }
}

# Path tree: /.well-known/{jmap,oauth-protected-resource}, /jmap,
# /download/{accountId}/{blobId}/{name}, /upload/{accountId}, /eventsource.
resource "aws_api_gateway_resource" "well_known" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_rest_api.this.root_resource_id
  path_part   = ".well-known"
}

resource "aws_api_gateway_resource" "session" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_resource.well_known.id
  path_part   = "jmap"
}

resource "aws_api_gateway_resource" "jmap" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_rest_api.this.root_resource_id
  path_part   = "jmap"
}

resource "aws_api_gateway_resource" "download" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_rest_api.this.root_resource_id
  path_part   = "download"
}

resource "aws_api_gateway_resource" "download_account" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_resource.download.id
  path_part   = "{accountId}"
}

resource "aws_api_gateway_resource" "download_blob" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_resource.download_account.id
  path_part   = "{blobId}"
}

resource "aws_api_gateway_resource" "download_name" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_resource.download_blob.id
  path_part   = "{name}"
}

resource "aws_api_gateway_resource" "upload" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_rest_api.this.root_resource_id
  path_part   = "upload"
}

resource "aws_api_gateway_resource" "upload_account" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_resource.upload.id
  path_part   = "{accountId}"
}

resource "aws_api_gateway_resource" "eventsource" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_rest_api.this.root_resource_id
  path_part   = "eventsource"
}

locals {
  routes = {
    session     = { resource = aws_api_gateway_resource.session.id, method = "GET", fn = "api", path = "GET/.well-known/jmap" }
    jmap        = { resource = aws_api_gateway_resource.jmap.id, method = "POST", fn = "api", path = "POST/jmap" }
    upload      = { resource = aws_api_gateway_resource.upload_account.id, method = "POST", fn = "api", path = "POST/upload/*" }
    download    = { resource = aws_api_gateway_resource.download_name.id, method = "GET", fn = "download", path = "GET/download/*" }
    eventsource = { resource = aws_api_gateway_resource.eventsource.id, method = "GET", fn = "push", path = "GET/eventsource" }
  }
}

resource "aws_api_gateway_method" "route" {
  for_each = local.routes

  rest_api_id   = aws_api_gateway_rest_api.this.id
  resource_id   = each.value.resource
  http_method   = each.value.method
  authorization = "CUSTOM"
  authorizer_id = aws_api_gateway_authorizer.bearer.id
}

# Every integration streams from the function's live alias. The 29 s
# default still caps total response time in STREAM mode, so it is raised
# to the 15-minute maximum (docs/evidence/phase0-streaming-spike.md).
resource "aws_api_gateway_integration" "route" {
  for_each = local.routes

  rest_api_id             = aws_api_gateway_rest_api.this.id
  resource_id             = each.value.resource
  http_method             = aws_api_gateway_method.route[each.key].http_method
  type                    = "AWS_PROXY"
  integration_http_method = "POST"
  uri                     = "arn:aws:apigateway:${local.region}:lambda:path/2021-11-15/functions/${aws_lambda_alias.live[each.value.fn].arn}/response-streaming-invocations"
  response_transfer_mode  = "STREAM"
  timeout_milliseconds    = 900000
}

# Each function may be invoked only by its own routes.
resource "aws_lambda_permission" "route" {
  for_each = local.routes

  statement_id   = "AllowJmapApi-${each.key}"
  action         = "lambda:InvokeFunction"
  function_name  = aws_lambda_function.fn[each.value.fn].function_name
  qualifier      = aws_lambda_alias.live[each.value.fn].name
  principal      = "apigateway.amazonaws.com"
  source_arn     = "${aws_api_gateway_rest_api.this.execution_arn}/*/${each.value.path}"
  source_account = data.aws_caller_identity.current.account_id
}

# --- Protected resource metadata (RFC 9728), public ------------------------

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
  # With binary_media_types = */* every request counts as binary, which
  # would skip the template; converting to text applies it again.
  content_handling = "CONVERT_TO_TEXT"
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
  # See the integration: keep the templated body text under */*.
  content_handling = "CONVERT_TO_TEXT"

  response_parameters = {
    "method.response.header.Cache-Control" = "'max-age=300'"
  }
  response_templates = {
    "application/json" = jsonencode({
      resource                 = local.base_url
      authorization_servers    = [var.issuer]
      scopes_supported         = var.scopes
      bearer_methods_supported = ["header"]
    })
  }

  depends_on = [aws_api_gateway_integration.metadata]
}

# --- Deployment --------------------------------------------------------------

resource "aws_api_gateway_deployment" "this" {
  rest_api_id = aws_api_gateway_rest_api.this.id

  triggers = {
    redeploy = sha1(jsonencode([
      aws_api_gateway_resource.well_known,
      aws_api_gateway_resource.session,
      aws_api_gateway_resource.jmap,
      aws_api_gateway_resource.download_name,
      aws_api_gateway_resource.upload_account,
      aws_api_gateway_resource.eventsource,
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

resource "aws_api_gateway_stage" "this" {
  rest_api_id   = aws_api_gateway_rest_api.this.id
  deployment_id = aws_api_gateway_deployment.this.id
  stage_name    = var.stage_name
}

# Initial interactive limits from docs/specs/security.md.
resource "aws_api_gateway_method_settings" "all" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  stage_name  = aws_api_gateway_stage.this.stage_name
  method_path = "*/*"

  settings {
    throttling_rate_limit  = 5
    throttling_burst_limit = 10
  }
}
