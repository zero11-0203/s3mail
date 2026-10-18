# Mail agent A0 (docs/specs/agent.md): an AgentCore runtime hosting the
# LangGraph image, Memory for its checkpoints and an AgentCore Identity
# credential provider for the owner's Cognito tokens. Mail is reached only
# through JMAP with those tokens, so the runtime role has no grant on any
# mail bucket, table, queue, key or index.
#
# Memory, the key, the client secret and the credential provider exist
# before an image does; the role, runtime and live endpoint only with one.

data "aws_region" "current" {}

data "aws_caller_identity" "current" {}

locals {
  region   = data.aws_region.current.region
  account  = data.aws_caller_identity.current.account_id
  arn      = "arn:aws:bedrock-agentcore:${local.region}:${local.account}"
  deployed = var.image_uri != null
}

# --- Agent stores --------------------------------------------------------------

# One customer-managed key for Memory and the client secret.
resource "aws_kms_key" "this" {
  description             = "${var.name} Memory and client secret"
  enable_key_rotation     = true
  deletion_window_in_days = 7
}

resource "aws_kms_alias" "this" {
  name          = "alias/${var.resource_prefix}"
  target_key_id = aws_kms_key.this.key_id
}

# Graph checkpoints: messages, capped mail excerpts, nothing authoritative.
# Short-term events only; no extraction strategy (and so no memory
# execution role) until owner preferences in A2.
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
        Sid      = "InvokePinnedModels"
        Effect   = "Allow"
        Action   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
        Resource = var.model_arns
      },
      {
        Sid      = "Checkpoints"
        Effect   = "Allow"
        Action   = ["bedrock-agentcore:CreateEvent", "bedrock-agentcore:GetEvent", "bedrock-agentcore:ListEvents", "bedrock-agentcore:DeleteEvent", "bedrock-agentcore:ListSessions"]
        Resource = aws_bedrockagentcore_memory.this.arn
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
        Sid      = "UseAgentKey"
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:GenerateDataKey"]
        Resource = aws_kms_key.this.arn
        Condition = {
          StringEquals = {
            "kms:ViaService" = [
              "secretsmanager.${local.region}.amazonaws.com",
              "bedrock-agentcore.${local.region}.amazonaws.com",
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
  description        = "s3mail read-only mail agent (A0)"
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
    JMAP_SESSION_URL    = var.jmap_session_url
    OAUTH_PROVIDER_NAME = aws_bedrockagentcore_oauth2_credential_provider.cognito.name
    OAUTH_SCOPES        = join(" ", var.oauth_scopes)
    OAUTH_RETURN_URL    = var.oauth_return_url
    MEMORY_ID           = aws_bedrockagentcore_memory.this.id
    ASK_MODEL_ID        = var.ask_model_id

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

# Releases move live to the new version after a smoke test; rollback moves
# it back (docs/specs/agent.md#deployment-and-operations).
resource "aws_bedrockagentcore_agent_runtime_endpoint" "live" {
  count = local.deployed ? 1 : 0

  name                  = "live"
  agent_runtime_id      = aws_bedrockagentcore_agent_runtime.this[0].agent_runtime_id
  agent_runtime_version = coalesce(var.live_version, aws_bedrockagentcore_agent_runtime.this[0].agent_runtime_version)
}
