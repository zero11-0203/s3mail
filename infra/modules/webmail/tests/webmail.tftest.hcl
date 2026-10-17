# Offline checks of the webmail hosting contract
# (docs/specs/clients.md#browser-hosting-and-client-safety).

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "111111111111" }
  }

  mock_data "aws_region" {
    defaults = { region = "us-east-1" }
  }

  mock_resource "aws_api_gateway_rest_api" {
    defaults = {
      id               = "abc123"
      root_resource_id = "root123"
      execution_arn    = "arn:aws:execute-api:us-east-1:111111111111:abc123"
    }
  }

  mock_resource "aws_lambda_alias" {
    defaults = { arn = "arn:aws:lambda:us-east-1:111111111111:function:s3mail-webmail:live" }
  }

  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::111111111111:role/s3mail/s3mail-webmail" }
  }

  mock_resource "aws_cloudwatch_log_group" {
    defaults = { arn = "arn:aws:logs:us-east-1:111111111111:log-group:/aws/lambda/s3mail-webmail" }
  }

  mock_resource "aws_acm_certificate" {
    defaults = {
      arn = "arn:aws:acm:us-east-1:111111111111:certificate/00000000-0000-4000-8000-000000000000"
      domain_validation_options = [{
        domain_name           = "webmail.example.com"
        resource_record_name  = "_x.webmail.example.com."
        resource_record_type  = "CNAME"
        resource_record_value = "_y.acm-validations.aws."
      }]
    }
  }

  mock_resource "aws_acm_certificate_validation" {
    defaults = { certificate_arn = "arn:aws:acm:us-east-1:111111111111:certificate/00000000-0000-4000-8000-000000000000" }
  }

  mock_resource "aws_api_gateway_domain_name" {
    defaults = { regional_domain_name = "d-abc123.execute-api.us-east-1.amazonaws.com" }
  }
}

variables {
  image_uri       = "111111111111.dkr.ecr.us-east-1.amazonaws.com/s3mail-webmail@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  domain          = "example.com"
  subdomain       = "webmail"
  zone_id         = "Z0000000000000"
  jmap_server_url = "https://abc.execute-api.us-east-1.amazonaws.com/v1"
  oauth_issuer    = "https://cognito-idp.us-east-1.amazonaws.com/us-east-1_example"
  oauth_client_id = "webmailclient"
  oauth_scopes    = ["s3mail/mail.read", "s3mail/mail.write", "s3mail/mail.submit"]
}

run "role_writes_only_logs_and_traces" {
  command = apply

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.this.policy).Statement :
    alltrue([for a in flatten([s.Action]) : can(regex("^(logs:(CreateLogStream|PutLogEvents)|xray:Put(TraceSegments|TelemetryRecords))$", a))])])
    error_message = "the webmail role has no mail, index, secret or ECR grants"
  }
}

run "function_is_bounded_oauth_only_and_streams" {
  command = apply

  assert {
    condition     = aws_lambda_function.this.package_type == "Image" && aws_lambda_function.this.reserved_concurrent_executions == 20
    error_message = "the image runs with bounded concurrency"
  }

  assert {
    condition = (
      aws_lambda_function.this.environment[0].variables["OAUTH_ONLY"] == "true" &&
      aws_lambda_function.this.environment[0].variables["AWS_LWA_INVOKE_MODE"] == "response_stream" &&
      aws_lambda_function.this.environment[0].variables["OAUTH_SCOPES"] == "s3mail/mail.read s3mail/mail.write s3mail/mail.submit" &&
      !contains(keys(aws_lambda_function.this.environment[0].variables), "SESSION_SECRET") &&
      !contains(keys(aws_lambda_function.this.environment[0].variables), "OAUTH_CLIENT_SECRET")
    )
    error_message = "OAuth-only with exact scopes, no stored-credential secret, streaming adapter"
  }

  assert {
    condition     = alltrue([for i in aws_api_gateway_integration.route : i.response_transfer_mode == "STREAM" && endswith(i.uri, ":live/response-streaming-invocations")])
    error_message = "both routes stream from the live alias"
  }

  assert {
    condition     = aws_api_gateway_rest_api.this.disable_execute_api_endpoint
    error_message = "only the custom domain serves the webmail"
  }

  assert {
    condition     = aws_api_gateway_domain_name.this.security_policy == "TLS_1_2" && aws_route53_record.webmail.type == "CNAME" && aws_route53_record.webmail.name == "webmail.example.com"
    error_message = "the webmail host is a TLS 1.2 custom domain behind a portable CNAME"
  }
}

run "image_must_be_pinned_by_digest" {
  command = plan

  variables {
    image_uri = "111111111111.dkr.ecr.us-east-1.amazonaws.com/s3mail-webmail:latest"
  }

  expect_failures = [var.image_uri]
}

run "verbose_logs_are_rejected" {
  command = plan

  variables {
    log_level = "debug"
  }

  expect_failures = [var.log_level]
}
