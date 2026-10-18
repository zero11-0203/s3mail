# Mail agent (docs/specs/agent.md): an AgentCore runtime hosting the
# LangGraph image, Memory for its checkpoints and the owner's preferences,
# an AgentCore Identity credential provider for the owner's Cognito tokens,
# the agent's own table, and from A1 the EmailReceived rule, queue and
# trigger Lambda. Mail is reached only through JMAP with those tokens, so no
# agent role has a grant on any mail bucket, table, queue, key or index.
#
# Memory, the key, both secrets, the table and the credential provider
# exist before an image does; the role, runtime and live endpoint only with
# one, and the trigger only with a runtime, a trigger zip and the event bus.

data "aws_region" "current" {}

data "aws_caller_identity" "current" {}

locals {
  region   = data.aws_region.current.region
  account  = data.aws_caller_identity.current.account_id
  arn      = "arn:aws:bedrock-agentcore:${local.region}:${local.account}"
  deployed = var.image_uri != null
  trigger  = local.deployed && var.trigger_zip != null && var.event_bus_name != null
}

# --- Agent stores --------------------------------------------------------------

# One customer-managed key for Memory and both secrets.
resource "aws_kms_key" "this" {
  description             = "${var.name} Memory and secrets"
  enable_key_rotation     = true
  deletion_window_in_days = 7
}

resource "aws_kms_alias" "this" {
  name          = "alias/${var.resource_prefix}"
  target_key_id = aws_kms_key.this.key_id
}

# Graph checkpoints (short-term events: messages, capped mail excerpts) and
# the owner's preferences (long-term records the owner writes with agent
# remember). Nothing authoritative, and no extraction strategy, so no
# memory execution role: nothing is extracted from mail.
resource "aws_bedrockagentcore_memory" "this" {
  name                  = "${var.name}_memory"
  description           = "LangGraph checkpoints for ${var.name}"
  event_expiry_duration = var.memory_expiry_days
  encryption_key_arn    = aws_kms_key.this.arn
}

# Holds {"client_secret": "..."} for the agent's Cognito client. Terraform
# creates it empty; the owner writes the value out of band
# (docs/operations/deployment.md#agent-deployment), so no secret is in state.
resource "aws_secretsmanager_secret" "client" {
  name                    = "${var.resource_prefix}/cognito-client"
  description             = "Client secret of the agent's Cognito app client, set by the owner"
  kms_key_id              = aws_kms_key.this.arn
  recovery_window_in_days = 7
}

# Holds {"api_key": "..."} for the model API (OpenRouter), set by the owner
# out of band like the client secret; the runtime reads it on first use.
resource "aws_secretsmanager_secret" "model_key" {
  name                    = "${var.resource_prefix}/model-api-key"
  description             = "Model API key (OpenRouter) for the agent, set by the owner"
  kms_key_id              = aws_kms_key.this.arn
  recovery_window_in_days = 7
}

resource "aws_bedrockagentcore_oauth2_credential_provider" "cognito" {
  name                       = "${var.resource_prefix}-cognito"
  credential_provider_vendor = "CustomOauth2"

  oauth2_provider_config {
    custom_oauth2_provider_config {
      client_id                    = var.oauth_client_id
      client_secret_source         = "EXTERNAL"
      client_authentication_method = "CLIENT_SECRET_BASIC"

      client_secret_config {
        secret_id = aws_secretsmanager_secret.client.arn
        json_key  = "client_secret"
      }

      oauth_discovery {
        discovery_url = "${var.oauth_issuer}/.well-known/openid-configuration"
      }
    }
  }
}

# The agent's own state (docs/specs/agent.md#state-and-memory): one triage
# row per email, the digest cursor and send markers. Derived and
# disposable: rows expire, and losing the table loses only triage labels,
# the cursor and send markers, so it has no backup.
resource "aws_dynamodb_table" "agent" {
  name         = var.resource_prefix
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"

  attribute {
    name = "pk"
    type = "S"
  }

  attribute {
    name = "sk"
    type = "S"
  }

  attribute {
    name = "reply_pk"
    type = "S"
  }

  attribute {
    name = "reply_sk"
    type = "S"
  }

  # Sparse: only triage rows that need a reply carry reply_pk.
  global_secondary_index {
    name            = "reply"
    hash_key        = "reply_pk"
    range_key       = "reply_sk"
    projection_type = "ALL"
  }

  ttl {
    attribute_name = "expires"
    enabled        = true
  }

  server_side_encryption {
    enabled     = true
    kms_key_arn = aws_kms_key.this.arn
  }
}

# --- Runtime role --------------------------------------------------------------

resource "aws_iam_role" "runtime" {
  count = local.deployed ? 1 : 0

  name = "${var.resource_prefix}-runtime"
  path = "/s3mail/"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "bedrock-agentcore.amazonaws.com" }
      Action    = "sts:AssumeRole"
      Condition = {
        StringEquals = { "aws:SourceAccount" = local.account }
        ArnLike      = { "aws:SourceArn" = "${local.arn}:*" }
      }
    }]
  })
}

# docs/operations/permissions.md#s3mail-agent-runtime-agentcore-runtime-execution-role
resource "aws_iam_role_policy" "runtime" {
  count = local.deployed ? 1 : 0

  name = "agent-runtime"
  role = aws_iam_role.runtime[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "PullImage"
        Effect   = "Allow"
        Action   = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"]
        Resource = var.repository_arn
      },
      {
        # ECR accepts no resource ARN for the registry token.
        Sid      = "EcrToken"
        Effect   = "Allow"
        Action   = "ecr:GetAuthorizationToken"
        Resource = "*"
      },
      {
        Sid      = "OwnLogGroups"
        Effect   = "Allow"
        Action   = ["logs:CreateLogGroup", "logs:DescribeLogStreams"]
        Resource = "arn:aws:logs:${local.region}:${local.account}:log-group:/aws/bedrock-agentcore/runtimes/${var.name}-*"
      },
      {
        Sid      = "WriteOwnLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "arn:aws:logs:${local.region}:${local.account}:log-group:/aws/bedrock-agentcore/runtimes/${var.name}-*:log-stream:*"
      },
      {
        # X-Ray accepts no resource ARNs for these actions.
        Sid      = "WriteTraces"
        Effect   = "Allow"
        Action   = ["xray:PutTraceSegments", "xray:PutTelemetryRecords", "xray:GetSamplingRules", "xray:GetSamplingTargets"]
        Resource = "*"
      },
      {
        Sid       = "PutMetrics"
        Effect    = "Allow"
        Action    = "cloudwatch:PutMetricData"
        Resource  = "*"
        Condition = { StringEquals = { "cloudwatch:namespace" = "bedrock-agentcore" } }
      },
      {
        Sid      = "Checkpoints"
        Effect   = "Allow"
        Action   = ["bedrock-agentcore:CreateEvent", "bedrock-agentcore:GetEvent", "bedrock-agentcore:ListEvents", "bedrock-agentcore:DeleteEvent", "bedrock-agentcore:ListSessions"]
        Resource = aws_bedrockagentcore_memory.this.arn
      },
      {
        # Owner-written preference records; nothing is extracted from mail.
        Sid      = "Preferences"
        Effect   = "Allow"
        Action   = ["bedrock-agentcore:BatchCreateMemoryRecords", "bedrock-agentcore:ListMemoryRecords", "bedrock-agentcore:BatchDeleteMemoryRecords"]
        Resource = aws_bedrockagentcore_memory.this.arn
      },
      {
        # No delete or scan: rows only expire.
        Sid      = "AgentTable"
        Effect   = "Allow"
        Action   = ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:Query", "dynamodb:BatchGetItem"]
        Resource = [aws_dynamodb_table.agent.arn, "${aws_dynamodb_table.agent.arn}/index/reply"]
      },
      {
        # Only the user-id form: callers are SigV4 (the owner's CLI), and
        # no inbound JWT exists before webmail chat.
        Sid    = "WorkloadToken"
        Effect = "Allow"
        Action = ["bedrock-agentcore:GetWorkloadAccessToken", "bedrock-agentcore:GetWorkloadAccessTokenForUserId"]
        Resource = [
          "${local.arn}:workload-identity-directory/default",
          "${local.arn}:workload-identity-directory/default/workload-identity/${var.name}-*",
        ]
      },
      {
        Sid    = "OwnerToken"
        Effect = "Allow"
        Action = "bedrock-agentcore:GetResourceOauth2Token"
        Resource = [
          "${local.arn}:token-vault/default",
          aws_bedrockagentcore_oauth2_credential_provider.cognito.credential_provider_arn,
          "${local.arn}:workload-identity-directory/default",
          "${local.arn}:workload-identity-directory/default/workload-identity/${var.name}-*",
        ]
      },
      {
        Sid      = "ReadClientSecret"
        Effect   = "Allow"
        Action   = "secretsmanager:GetSecretValue"
        Resource = aws_secretsmanager_secret.client.arn
      },
      {
        # Models are reached over HTTPS through OpenRouter, so the role has
        # no Bedrock grant; only the owner-set API key is read.
        Sid      = "ReadModelKey"
        Effect   = "Allow"
        Action   = "secretsmanager:GetSecretValue"
        Resource = aws_secretsmanager_secret.model_key.arn
      },
      {
        # AgentCore checks the Memory's key with the caller's credentials
        # when long-term records (preferences) are read or written.
        Sid      = "UseAgentKey"
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:GenerateDataKey", "kms:DescribeKey"]
        Resource = aws_kms_key.this.arn
        Condition = {
          StringEquals = {
            "kms:ViaService" = [
              "secretsmanager.${local.region}.amazonaws.com",
              "bedrock-agentcore.${local.region}.amazonaws.com",
              "dynamodb.${local.region}.amazonaws.com",
            ]
          }
        }
      },
    ]
  })
}

# --- Runtime -------------------------------------------------------------------

resource "aws_bedrockagentcore_agent_runtime" "this" {
  count = local.deployed ? 1 : 0

  agent_runtime_name = var.name
  description        = "s3mail mail agent"
  role_arn           = aws_iam_role.runtime[0].arn

  agent_runtime_artifact {
    container_configuration {
      container_uri = var.image_uri
    }
  }

  network_configuration {
    network_mode = "PUBLIC"
  }

  # SigV4 inbound only: no authorizer_configuration until webmail chat.
  protocol_configuration {
    server_protocol = "HTTP"
  }

  lifecycle_configuration = [{
    idle_runtime_session_timeout = var.idle_session_seconds
    max_lifetime                 = var.max_session_seconds
  }]

  environment_variables = {
    JMAP_SESSION_URL     = var.jmap_session_url
    OAUTH_PROVIDER_NAME  = aws_bedrockagentcore_oauth2_credential_provider.cognito.name
    OAUTH_SCOPES         = join(" ", var.oauth_scopes)
    OAUTH_RETURN_URL     = var.oauth_return_url
    MEMORY_ID            = aws_bedrockagentcore_memory.this.id
    ASK_MODEL            = var.ask_model
    TRIAGE_MODEL         = var.triage_model
    MODEL_BASE_URL       = var.model_base_url
    MODEL_KEY_SECRET_ARN = aws_secretsmanager_secret.model_key.arn
    AGENT_TABLE          = aws_dynamodb_table.agent.name
    AGENT_FEATURES       = join(" ", var.features)

    # AgentCore Observability without content: ADOT's framework
    # instrumentors record prompts and completions whatever the capture
    # flag says, so they stay off; the agent emits its own content-free
    # spans, and its stdout logs reach the runtime log group directly.
    AGENT_OBSERVABILITY_ENABLED                        = "true"
    OTEL_PYTHON_DISTRO                                 = "aws_distro"
    OTEL_PYTHON_CONFIGURATOR                           = "aws_configurator"
    OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT = "false"
    OTEL_PYTHON_DISABLED_INSTRUMENTATIONS              = "aws_langchain,aws_crewai,aws_llama-index,aws_openai_agents,aws_mcp"
    OTEL_LOGS_EXPORTER                                 = "none"
    OTEL_RESOURCE_ATTRIBUTES                           = "service.name=${var.name}"
  }

  depends_on = [aws_iam_role_policy.runtime]
}

# live serves only the approved version. A new image or setting creates a
# candidate version, which the DEFAULT endpoint serves for the smoke test;
# promotion and rollback set live_version
# (docs/operations/deployment.md#agent-deployment).
resource "aws_bedrockagentcore_agent_runtime_endpoint" "live" {
  count = local.deployed ? 1 : 0

  name                  = "live"
  agent_runtime_id      = aws_bedrockagentcore_agent_runtime.this[0].agent_runtime_id
  agent_runtime_version = var.live_version
}

# The runtime writes one log group per endpoint. Terraform owns them so
# retention is bounded (docs/operations/monitoring.md); existing groups are
# imported once (docs/operations/deployment.md#agent-deployment).
resource "aws_cloudwatch_log_group" "runtime" {
  for_each = local.deployed ? toset(["DEFAULT", "live"]) : toset([])

  name              = "/aws/bedrock-agentcore/runtimes/${aws_bedrockagentcore_agent_runtime.this[0].agent_runtime_id}-${each.key}"
  retention_in_days = var.log_retention_days
}
