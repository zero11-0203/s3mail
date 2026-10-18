# Offline checks of the search pipeline's least-privilege and lifecycle
# contracts (docs/specs/search.md#acceptance-checks).

mock_provider "aws" {
  mock_resource "aws_lambda_alias" {
    defaults = { arn = "arn:aws:lambda:us-east-1:111111111111:function:s3mail-search:live" }
  }

  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::111111111111:role/s3mail/s3mail-search" }
  }

  mock_resource "aws_cloudwatch_log_group" {
    defaults = { arn = "arn:aws:logs:us-east-1:111111111111:log-group:/aws/lambda/s3mail-search" }
  }

  mock_resource "aws_s3_bucket" {
    defaults = { arn = "arn:aws:s3:::s3mail-index-x", id = "s3mail-index-x" }
  }

  mock_resource "aws_secretsmanager_secret" {
    defaults = { arn = "arn:aws:secretsmanager:us-east-1:111111111111:secret:s3mail/embeddings-api-key-x" }
  }

  mock_resource "aws_sqs_queue" {
    defaults = { arn = "arn:aws:sqs:us-east-1:111111111111:s3mail-index", url = "https://sqs.us-east-1.amazonaws.com/111111111111/s3mail-index" }
  }
}

variables {
  indexer_zip          = "tests/fake-lambda.txt"
  search_zip           = "tests/fake-lambda.txt"
  table_name           = "s3mail-mail"
  table_arn            = "arn:aws:dynamodb:us-east-1:111111111111:table/s3mail-mail"
  account_id           = "primary"
  account_partition    = "ACCOUNT#primary"
  originals_bucket_arn = "arn:aws:s3:::s3mail-originals-x"
}

run "least_privilege" {
  command = apply

  assert {
    condition     = length(aws_secretsmanager_secret.embed_key) == 0 && alltrue([for f in aws_lambda_function.fn : !contains(keys(f.environment[0].variables), "EMBED_MODEL")]) && !output.semantic
    error_message = "semantic search is off by default"
  }

  assert {
    condition     = aws_lambda_function.fn["indexer"].reserved_concurrent_executions == 1
    error_message = "the indexer is the single writer"
  }

  assert {
    condition     = alltrue([for f in aws_lambda_function.fn : one(f.tracing_config).mode == "Active"])
    error_message = "every search function is traced"
  }

  assert {
    condition = alltrue([for k, p in aws_iam_role_policy.fn : alltrue([
      for st in jsondecode(p.policy).Statement : !anytrue([
        for a in flatten([st.Action]) : contains(["dynamodb:DeleteItem", "s3:DeleteObject", "s3:DeleteObjectVersion", "s3:*", "dynamodb:*"], a)
      ])
    ])])
    error_message = "no search role may delete records, originals or index versions"
  }

  assert {
    condition = alltrue([for k in ["search"] : alltrue([
      for st in jsondecode(aws_iam_role_policy.fn[k].policy).Statement : !anytrue([
        for a in flatten([st.Action]) : contains(["dynamodb:PutItem", "dynamodb:UpdateItem", "s3:PutObject"], a)
      ])
    ])])
    error_message = "only the indexer writes"
  }

  assert {
    condition = alltrue([for k in ["search"] : alltrue([
      for st in jsondecode(aws_iam_role_policy.fn[k].policy).Statement :
      !anytrue([for r in flatten([st.Resource]) : startswith(r, var.originals_bucket_arn)])
    ])])
    error_message = "only the indexer reads mail content"
  }

  assert {
    condition = alltrue([for k, p in aws_iam_role_policy.fn : alltrue([
      for st in jsondecode(p.policy).Statement :
      !startswith(flatten([st.Action])[0], "dynamodb:") || st.Condition["ForAllValues:StringEquals"]["dynamodb:LeadingKeys"] == ["ACCOUNT#primary"]
    ])])
    error_message = "table access is limited to the account partition"
  }

  assert {
    condition     = one(aws_s3_bucket_versioning.index.versioning_configuration).status == "Enabled"
    error_message = "readers fetch exact index versions"
  }

  assert {
    condition = (
      one(one(aws_s3_bucket_lifecycle_configuration.index.rule).noncurrent_version_expiration).noncurrent_days == 1 &&
      one(one(aws_s3_bucket_lifecycle_configuration.index.rule).noncurrent_version_expiration).newer_noncurrent_versions == 3
    )
    error_message = "old generations expire after a day, keeping three"
  }
}

run "semantic_reads_only_its_key" {
  command = apply

  variables {
    embedding_model = "openai/text-embedding-3-small"
  }

  assert {
    condition = alltrue([for f in aws_lambda_function.fn :
      f.environment[0].variables["EMBED_MODEL"] == var.embedding_model &&
      f.environment[0].variables["EMBED_KEY_SECRET"] == aws_secretsmanager_secret.embed_key[0].arn
    ]) && output.semantic
    error_message = "both functions embed with the model and its key"
  }

  assert {
    condition = alltrue([for k, p in aws_iam_role_policy.fn : alltrue([
      for st in jsondecode(p.policy).Statement : st.Resource == aws_secretsmanager_secret.embed_key[0].arn
      if anytrue([for a in flatten([st.Action]) : startswith(a, "secretsmanager:")])
    ]) && anytrue([for st in jsondecode(p.policy).Statement : st.Action == "secretsmanager:GetSecretValue"])])
    error_message = "each role reads only the embeddings key"
  }

  assert {
    condition = alltrue([for k in ["search"] : alltrue([
      for st in jsondecode(aws_iam_role_policy.fn[k].policy).Statement :
      !anytrue([for r in flatten([st.Resource]) : startswith(r, var.originals_bucket_arn)])
    ])])
    error_message = "the search function still reads no mail content"
  }
}
