# Offline checks of the JMAP API's authorization and least-privilege
# contracts (docs/specs/security.md#acceptance-checks).

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "111111111111" }
  }

  mock_data "aws_region" {
    defaults = { region = "us-east-1" }
  }

  mock_resource "aws_api_gateway_rest_api" {
    defaults = {
      id            = "abc123"
      execution_arn = "arn:aws:execute-api:us-east-1:111111111111:abc123"
    }
  }

  mock_resource "aws_lambda_alias" {
    defaults = { arn = "arn:aws:lambda:us-east-1:111111111111:function:s3mail-jmap:live" }
  }

  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::111111111111:role/s3mail/s3mail-jmap" }
  }

  mock_resource "aws_cloudwatch_log_group" {
    defaults = { arn = "arn:aws:logs:us-east-1:111111111111:log-group:/aws/lambda/s3mail-jmap" }
  }
}

variables {
  api_zip               = "tests/fake-lambda.txt"
  download_zip          = "tests/fake-lambda.txt"
  push_zip              = "tests/fake-lambda.txt"
  authorizer_zip        = "tests/fake-lambda.txt"
  issuer                = "https://cognito-idp.us-east-1.amazonaws.com/us-east-1_example"
  client_ids            = ["exampleclient"]
  scopes                = ["s3mail/mail.read", "s3mail/mail.write", "s3mail/mail.submit"]
  owner_subject         = "00000000-0000-4000-8000-000000000000"
  table_name            = "s3mail-mail"
  table_arn             = "arn:aws:dynamodb:us-east-1:111111111111:table/s3mail-mail"
  account_id            = "primary"
  account_partition     = "ACCOUNT#primary"
  originals_bucket_name = "s3mail-originals-test"
  originals_bucket_arn  = "arn:aws:s3:::s3mail-originals-test"
  ses_key_prefix        = "inbound/"
}

run "every_jmap_route_requires_the_bearer_authorizer" {
  command = apply

  assert {
    condition     = alltrue([for m in aws_api_gateway_method.route : m.authorization == "CUSTOM" && m.authorizer_id == aws_api_gateway_authorizer.bearer.id])
    error_message = "every JMAP route uses the bearer authorizer"
  }

  assert {
    condition     = aws_api_gateway_authorizer.bearer.authorizer_result_ttl_in_seconds == 0
    error_message = "authorizer results are never cached"
  }

  assert {
    condition     = aws_api_gateway_method.metadata.authorization == "NONE" && aws_api_gateway_integration.metadata.type == "MOCK"
    error_message = "only the static resource metadata is public"
  }

  assert {
    condition     = aws_api_gateway_integration.metadata.content_handling == "CONVERT_TO_TEXT"
    error_message = "the MOCK template must still apply with binary_media_types = */*"
  }

  assert {
    condition     = alltrue([for i in aws_api_gateway_integration.route : i.response_transfer_mode == "STREAM" && i.timeout_milliseconds == 900000])
    error_message = "integrations stream with the 15-minute timeout"
  }
}

run "roles_are_least_privilege" {
  command = apply

  assert {
    condition = alltrue([for k, p in aws_iam_role_policy.fn : alltrue([
      for s in jsondecode(p.policy).Statement : !anytrue([for a in flatten([s.Action]) : can(regex("^s3:Delete|^dynamodb:(Scan|\\*)|\\*$", a))])
    ])])
    error_message = "no JMAP role deletes S3 objects, scans or uses wildcards"
  }

  assert {
    condition = alltrue([for k, p in aws_iam_role_policy.fn : alltrue([
      for s in jsondecode(p.policy).Statement : k == "api" && alltrue([
        for r in flatten([s.Resource]) : can(regex("^arn:aws:s3:::s3mail-originals-test/(imported|composed)/\\*$", r)) || endswith(r, "/uploads/*")
      ])
      if anytrue([for a in flatten([s.Action]) : startswith(a, "s3:Put")])
    ])])
    error_message = "only the API role writes S3, and only new originals under imported/ and composed/ or uploads"
  }

  assert {
    condition     = one([for r in aws_s3_bucket_lifecycle_configuration.blobs.rule : r.expiration[0].days]) == 1
    error_message = "uploads expire from the disposable bucket"
  }

  assert {
    condition = alltrue([for k, p in aws_iam_role_policy.fn : alltrue([
      for s in jsondecode(p.policy).Statement : s.Condition["ForAllValues:StringEquals"]["dynamodb:LeadingKeys"] == ["ACCOUNT#primary"]
      if anytrue([for a in flatten([s.Action]) : startswith(a, "dynamodb:")])
    ])])
    error_message = "every table grant is limited to the account partition"
  }

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.fn["push"].policy).Statement :
    s.Action == "dynamodb:GetItem" if s.Sid == "ReadOwnAccountState"]) && length(jsondecode(aws_iam_role_policy.fn["push"].policy).Statement) == 3
    error_message = "push reads STATE only and has no S3 access"
  }

  assert {
    condition = !anytrue([for s in jsondecode(aws_iam_role_policy.fn["download"].policy).Statement :
    anytrue([for a in flatten([s.Action]) : contains(["dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem"], a)])])
    error_message = "download is read-only"
  }

  assert {
    condition     = aws_lambda_function.fn["push"].reserved_concurrent_executions == 4
    error_message = "push concurrency is capped"
  }
}

run "only_the_api_invokes_search" {
  command = apply

  variables {
    search_function_arn = "arn:aws:lambda:us-east-1:111111111111:function:s3mail-search-search:live"
  }

  assert {
    condition = alltrue([for k, p in aws_iam_role_policy.fn : alltrue([
      for s in jsondecode(p.policy).Statement : k == "api" && s.Resource == var.search_function_arn
      if anytrue([for a in flatten([s.Action]) : startswith(a, "lambda:")])
    ])]) && anytrue([for s in jsondecode(aws_iam_role_policy.fn["api"].policy).Statement : s.Action == "lambda:InvokeFunction"])
    error_message = "only the API role invokes, and only the search alias"
  }

  assert {
    condition     = aws_lambda_function.fn["api"].environment[0].variables["SEARCH_FUNCTION"] == var.search_function_arn && !contains(keys(aws_lambda_function.fn["download"].environment[0].variables), "SEARCH_FUNCTION")
    error_message = "only the API function is configured for search"
  }
}

run "owner_subject_must_be_a_uuid" {
  command = plan

  variables {
    owner_subject = "owner"
  }

  expect_failures = [var.owner_subject]
}
