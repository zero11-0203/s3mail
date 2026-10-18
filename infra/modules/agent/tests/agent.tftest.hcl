# Offline checks of the agent's contract (docs/specs/agent.md).

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "111111111111" }
  }

  mock_data "aws_region" {
    defaults = { region = "us-east-1" }
  }

  mock_resource "aws_kms_key" {
    defaults = { arn = "arn:aws:kms:us-east-1:111111111111:key/00000000-0000-4000-8000-000000000000" }
  }

  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::111111111111:role/s3mail/s3mail-agent-runtime" }
  }

  mock_resource "aws_secretsmanager_secret" {
    defaults = { arn = "arn:aws:secretsmanager:us-east-1:111111111111:secret:s3mail-agent/cognito-client-AbCdEf" }
  }

  mock_resource "aws_bedrockagentcore_memory" {
    defaults = {
      id  = "s3mail_agent_memory-abc"
      arn = "arn:aws:bedrock-agentcore:us-east-1:111111111111:memory/s3mail_agent_memory-abc"
    }
  }

  mock_resource "aws_bedrockagentcore_oauth2_credential_provider" {
    defaults = {
      credential_provider_arn = "arn:aws:bedrock-agentcore:us-east-1:111111111111:token-vault/default/oauth2credentialprovider/s3mail-agent-cognito"
      callback_url            = "https://bedrock-agentcore.us-east-1.amazonaws.com/identities/oauth2/callback/00000000-0000-4000-8000-000000000000"
    }
  }

  mock_resource "aws_dynamodb_table" {
    defaults = { arn = "arn:aws:dynamodb:us-east-1:111111111111:table/s3mail-agent" }
  }

  mock_resource "aws_sqs_queue" {
    defaults = { arn = "arn:aws:sqs:us-east-1:111111111111:s3mail-agent-triage", url = "https://sqs.us-east-1.amazonaws.com/111111111111/s3mail-agent-triage" }
  }

  mock_resource "aws_cloudwatch_event_rule" {
    defaults = { arn = "arn:aws:events:us-east-1:111111111111:rule/s3mail-events/s3mail-agent-received" }
  }

  mock_resource "aws_cloudwatch_log_group" {
    defaults = { arn = "arn:aws:logs:us-east-1:111111111111:log-group:/aws/lambda/s3mail-agent-trigger" }
  }

  mock_resource "aws_lambda_function" {
    defaults = { arn = "arn:aws:lambda:us-east-1:111111111111:function:s3mail-agent-trigger" }
  }

  mock_resource "aws_bedrockagentcore_agent_runtime" {
    defaults = {
      agent_runtime_id      = "s3mail_agent-abc"
      agent_runtime_arn     = "arn:aws:bedrock-agentcore:us-east-1:111111111111:runtime/s3mail_agent-abc"
      agent_runtime_version = "1"
    }
  }
}

variables {
  image_uri        = "111111111111.dkr.ecr.us-east-1.amazonaws.com/s3mail-agent@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  repository_arn   = "arn:aws:ecr:us-east-1:111111111111:repository/s3mail-agent"
  jmap_session_url = "https://abc.execute-api.us-east-1.amazonaws.com/v1/.well-known/jmap"
  oauth_issuer     = "https://cognito-idp.us-east-1.amazonaws.com/us-east-1_example"
  oauth_client_id  = "agentclient"
  oauth_scopes     = ["s3mail/mail.read"]
  live_version     = "1"
  event_bus_name   = "s3mail-events"
  trigger_zip      = "tests/fake-lambda.txt"
}

run "runtime_role_has_no_mail_access" {
  command = apply

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.runtime[0].policy).Statement :
      alltrue([for a in flatten([s.Action]) : can(regex("^(ecr:(BatchGetImage|GetDownloadUrlForLayer|GetAuthorizationToken)|logs:(CreateLogGroup|DescribeLogStreams|CreateLogStream|PutLogEvents)|xray:(PutTraceSegments|PutTelemetryRecords|GetSamplingRules|GetSamplingTargets)|cloudwatch:PutMetricData|bedrock-agentcore:(CreateEvent|GetEvent|ListEvents|DeleteEvent|ListSessions|BatchCreateMemoryRecords|ListMemoryRecords|BatchDeleteMemoryRecords|GetWorkloadAccessToken|GetWorkloadAccessTokenForUserId|GetResourceOauth2Token)|dynamodb:(GetItem|PutItem|Query|BatchGetItem)|secretsmanager:GetSecretValue|kms:(Decrypt|GenerateDataKey))$", a))])
    ])
    error_message = "the runtime role grants only image, logs, traces, Memory, Identity, its own secrets and table: no Bedrock models, S3, SQS, index or mail access"
  }

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.runtime[0].policy).Statement :
      alltrue([for r in flatten([s.Resource]) : startswith(r, aws_dynamodb_table.agent.arn)]) if anytrue([for a in flatten([s.Action]) : startswith(a, "dynamodb:")])
    ])
    error_message = "the runtime's only table is the agent's own"
  }

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.runtime[0].policy).Statement :
      s.Resource != "*" if !contains(["EcrToken", "WriteTraces", "PutMetrics"], s.Sid)
    ])
    error_message = "only actions without resource ARNs use a wildcard resource"
  }

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.runtime[0].policy).Statement :
      contains([aws_secretsmanager_secret.client.arn, aws_secretsmanager_secret.model_key.arn], s.Resource) if s.Action == "secretsmanager:GetSecretValue"
    ])
    error_message = "the runtime reads only its own two secrets"
  }

  assert {
    condition     = !strcontains(aws_iam_role_policy.runtime[0].policy, "GetWorkloadAccessTokenForJWT")
    error_message = "no JWT workload tokens before an inbound JWT authorizer exists"
  }

  assert {
    condition     = jsondecode(aws_iam_role.runtime[0].assume_role_policy).Statement[0].Condition.StringEquals["aws:SourceAccount"] == "111111111111"
    error_message = "only this account's AgentCore may assume the runtime role"
  }
}

run "telemetry_is_content_free_and_lifecycle_bounded" {
  command = apply

  assert {
    condition = (
      aws_bedrockagentcore_agent_runtime.this[0].environment_variables["OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"] == "false" &&
      strcontains(aws_bedrockagentcore_agent_runtime.this[0].environment_variables["OTEL_PYTHON_DISABLED_INSTRUMENTATIONS"], "aws_langchain")
    )
    error_message = "GenAI message capture and the content-recording LangChain instrumentor are off"
  }

  assert {
    condition = (
      aws_bedrockagentcore_agent_runtime.this[0].lifecycle_configuration[0].idle_runtime_session_timeout == 900 &&
      aws_bedrockagentcore_agent_runtime.this[0].lifecycle_configuration[0].max_lifetime == 3600
    )
    error_message = "sessions idle out after 900 s and end after 3600 s"
  }

  assert {
    condition = (
      aws_bedrockagentcore_agent_runtime.this[0].environment_variables["ASK_MODEL"] == "openai/gpt-6.1-sol" &&
      aws_bedrockagentcore_agent_runtime.this[0].environment_variables["MODEL_KEY_SECRET_ARN"] == aws_secretsmanager_secret.model_key.arn &&
      !contains(keys(aws_bedrockagentcore_agent_runtime.this[0].environment_variables), "OPENROUTER_API_KEY")
    )
    error_message = "the runtime gets the pinned model and the key's ARN, never the key"
  }

  assert {
    condition     = aws_bedrockagentcore_agent_runtime.this[0].environment_variables["OAUTH_SCOPES"] == "s3mail/mail.read"
    error_message = "A0 requests mail.read only"
  }

  assert {
    condition = (
      aws_bedrockagentcore_agent_runtime.this[0].environment_variables["TRIAGE_MODEL"] == "openai/gpt-6-luna" &&
      aws_bedrockagentcore_agent_runtime.this[0].environment_variables["AGENT_TABLE"] == aws_dynamodb_table.agent.name &&
      aws_bedrockagentcore_agent_runtime.this[0].environment_variables["AGENT_FEATURES"] == ""
    )
    error_message = "triage uses its pinned model, and A4 tools are off by default"
  }

  assert {
    condition     = length(aws_bedrockagentcore_agent_runtime.this[0].authorizer_configuration) == 0
    error_message = "inbound is SigV4 only until webmail chat"
  }

  assert {
    condition     = aws_bedrockagentcore_agent_runtime_endpoint.live[0].name == "live"
    error_message = "callers use the live endpoint"
  }
}

run "stores_are_encrypted_and_expiring" {
  command = apply

  assert {
    condition     = aws_bedrockagentcore_memory.this.event_expiry_duration == 7 && aws_bedrockagentcore_memory.this.encryption_key_arn == aws_kms_key.this.arn
    error_message = "checkpoints expire after 7 days under the customer-managed key"
  }

  assert {
    condition = (
      aws_bedrockagentcore_oauth2_credential_provider.cognito.oauth2_provider_config[0].custom_oauth2_provider_config[0].client_secret_source == "EXTERNAL" &&
      aws_bedrockagentcore_oauth2_credential_provider.cognito.oauth2_provider_config[0].custom_oauth2_provider_config[0].client_secret == null
    )
    error_message = "the provider reads the owner-set secret; Terraform never holds it"
  }
}

run "agent_table_expires_and_uses_the_agent_key" {
  command = apply

  assert {
    condition = (
      aws_dynamodb_table.agent.ttl[0].attribute_name == "expires" && aws_dynamodb_table.agent.ttl[0].enabled &&
      aws_dynamodb_table.agent.server_side_encryption[0].kms_key_arn == aws_kms_key.this.arn &&
      length(aws_dynamodb_table.agent.point_in_time_recovery) == 0
    )
    error_message = "agent rows expire, use the agent key and have no backup"
  }
}

run "trigger_only_consumes_its_queue_and_invokes_live" {
  command = apply

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.trigger[0].policy).Statement :
      alltrue([for a in flatten([s.Action]) : can(regex("^(logs:(CreateLogStream|PutLogEvents)|sqs:(ReceiveMessage|DeleteMessage|GetQueueAttributes)|bedrock-agentcore:InvokeAgentRuntime(ForUser)?|xray:(PutTraceSegments|PutTelemetryRecords))$", a))])
    ])
    error_message = "the trigger has no mail, Memory, Identity, model or table access"
  }

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.trigger[0].policy).Statement :
      alltrue([for r in flatten([s.Resource]) : contains([aws_bedrockagentcore_agent_runtime.this[0].agent_runtime_arn, "${aws_bedrockagentcore_agent_runtime.this[0].agent_runtime_arn}/runtime-endpoint/live"], r)])
      if s.Sid == "InvokeLive"
    ])
    error_message = "the trigger invokes only this runtime's live endpoint"
  }

  assert {
    condition     = aws_lambda_function.trigger[0].environment[0].variables["QUALIFIER"] == "live"
    error_message = "triage runs only on the approved version"
  }

  assert {
    condition = (
      jsondecode(aws_cloudwatch_event_rule.received[0].event_pattern).source[0] == "s3mail.mail" &&
      jsondecode(aws_cloudwatch_event_rule.received[0].event_pattern)["detail-type"][0] == "EmailReceived" &&
      aws_cloudwatch_event_rule.received[0].event_bus_name == "s3mail-events"
    )
    error_message = "the rule matches only EmailReceived on the mail bus"
  }

  assert {
    condition     = jsondecode(aws_sqs_queue_policy.triage[0].policy).Statement[0].Condition.ArnEquals["aws:SourceArn"] == aws_cloudwatch_event_rule.received[0].arn
    error_message = "only the agent's rule may send to the triage queue"
  }

  assert {
    condition = (
      jsondecode(aws_sqs_queue.triage[0].redrive_policy).maxReceiveCount == 5 &&
      aws_sqs_queue.triage[0].visibility_timeout_seconds == 720 &&
      aws_lambda_event_source_mapping.triage[0].batch_size == 1 &&
      aws_lambda_event_source_mapping.triage[0].scaling_config[0].maximum_concurrency == 2 &&
      contains(aws_lambda_event_source_mapping.triage[0].function_response_types, "ReportBatchItemFailures")
    )
    error_message = "failed runs retry five times into the DLQ, one at a time, at most two at once"
  }

  assert {
    condition     = length(aws_cloudwatch_event_rule.digest) == 0
    error_message = "no digest is scheduled until mail.write is granted"
  }
}

run "digest_schedule_delivers_mail" {
  command = apply

  variables {
    digest_schedule = "cron(0 7 * * ? *)"
  }

  assert {
    condition     = jsondecode(aws_cloudwatch_event_target.digest[0].input).digest == "mail" && aws_lambda_permission.digest[0].source_arn == aws_cloudwatch_event_rule.digest[0].arn
    error_message = "only the digest rule invokes the trigger, with the digest input"
  }
}

run "no_trigger_without_the_bus" {
  command = plan

  variables {
    event_bus_name = null
  }

  assert {
    condition     = length(aws_lambda_function.trigger) == 0 && length(aws_sqs_queue.triage) == 0 && length(aws_cloudwatch_event_rule.received) == 0
    error_message = "A0 deployments create no trigger"
  }
}

run "features_are_known_tools" {
  command = plan

  variables {
    features = ["attachments", "browse"]
  }

  expect_failures = [var.features]
}

run "a_new_version_does_not_move_live" {
  command   = apply
  state_key = "release"

  # The runtime is at a new candidate version; live stays on the approved one.
  override_resource {
    target = aws_bedrockagentcore_agent_runtime.this[0]
    values = { agent_runtime_id = "s3mail_agent-abc", agent_runtime_version = "3" }
  }

  assert {
    condition     = aws_bedrockagentcore_agent_runtime_endpoint.live[0].agent_runtime_version == "1"
    error_message = "live stays on live_version"
  }

  assert {
    condition     = output.candidate_version == "3"
    error_message = "the candidate is the runtime's newest version"
  }
}

run "live_version_is_required_with_an_image" {
  command = plan

  variables {
    live_version = null
  }

  expect_failures = [var.live_version]
}

run "runtime_logs_have_bounded_retention" {
  command = apply

  assert {
    condition = (
      length(aws_cloudwatch_log_group.runtime) == 2 &&
      alltrue([for k, g in aws_cloudwatch_log_group.runtime : g.retention_in_days == 30 && g.name == "/aws/bedrock-agentcore/runtimes/s3mail_agent-abc-${k}"])
    )
    error_message = "both endpoint log groups are Terraform-owned with 30-day retention"
  }
}

run "no_runtime_without_an_image" {
  command = plan

  variables {
    image_uri    = null
    live_version = null
  }

  assert {
    condition     = length(aws_bedrockagentcore_agent_runtime.this) == 0 && length(aws_iam_role.runtime) == 0
    error_message = "the first apply creates only the stores and credential provider"
  }
}

run "image_must_be_pinned_by_digest" {
  command = plan

  variables {
    image_uri = "111111111111.dkr.ecr.us-east-1.amazonaws.com/s3mail-agent:latest"
  }

  expect_failures = [var.image_uri]
}

run "model_api_must_use_https" {
  command = plan

  variables {
    model_base_url = "http://openrouter.example.com/api/v1"
  }

  expect_failures = [var.model_base_url]
}
