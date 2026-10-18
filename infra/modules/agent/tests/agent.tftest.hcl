# Offline checks of the agent A0 contract (docs/specs/agent.md).

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
  ask_model_id     = "us.example.model-v1"
  model_arns = [
    "arn:aws:bedrock:us-east-1:111111111111:inference-profile/us.example.model-v1",
    "arn:aws:bedrock:us-east-1::foundation-model/example.model-v1",
  ]
}

run "runtime_role_has_no_mail_access" {
  command = apply

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.runtime[0].policy).Statement :
      alltrue([for a in flatten([s.Action]) : can(regex("^(ecr:(BatchGetImage|GetDownloadUrlForLayer|GetAuthorizationToken)|logs:(CreateLogGroup|DescribeLogStreams|CreateLogStream|PutLogEvents)|xray:(PutTraceSegments|PutTelemetryRecords|GetSamplingRules|GetSamplingTargets)|cloudwatch:PutMetricData|bedrock:InvokeModel(WithResponseStream)?|bedrock-agentcore:(CreateEvent|GetEvent|ListEvents|DeleteEvent|ListSessions|GetWorkloadAccessToken|GetWorkloadAccessTokenForUserId|GetResourceOauth2Token)|secretsmanager:GetSecretValue|kms:(Decrypt|GenerateDataKey))$", a))])
    ])
    error_message = "the runtime role grants only image, logs, traces, models, Memory, Identity and its own secret: no S3, DynamoDB, SQS or index access"
  }

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.runtime[0].policy).Statement :
      s.Resource != "*" if !contains(["EcrToken", "WriteTraces", "PutMetrics"], s.Sid)
    ])
    error_message = "only actions without resource ARNs use a wildcard resource"
  }

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.runtime[0].policy).Statement :
      jsonencode(s.Resource) == jsonencode(var.model_arns) if s.Sid == "InvokePinnedModels"
    ])
    error_message = "models are invoked only by their pinned ARNs"
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
    condition     = aws_bedrockagentcore_agent_runtime.this[0].environment_variables["OAUTH_SCOPES"] == "s3mail/mail.read"
    error_message = "A0 requests mail.read only"
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

run "no_runtime_without_an_image" {
  command = plan

  variables {
    image_uri = null
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

run "model_wildcards_are_rejected" {
  command = plan

  variables {
    model_arns = ["arn:aws:bedrock:*::foundation-model/*"]
  }

  expect_failures = [var.model_arns]
}
