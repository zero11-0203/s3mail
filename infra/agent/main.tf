# The optional mail agent (docs/specs/agent.md), a root configuration with
# its own state, applied separately from infra/main
# (docs/operations/deployment.md#agent-deployment). infra/main only
# registers the agent's Cognito app client and allowlist entry; this root
# never reads its state, and the mail system never depends on this one.
#
# Two steps, like infra/webmail: apply with image_digest unset to create the
# repository, Memory, secret and credential provider; push the image (make
# agent-image); then apply with its digest.

resource "aws_ecr_repository" "agent" {
  name                 = "${var.project}-agent"
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }
}

# Keep recent images so earlier digests stay available for rollback.
resource "aws_ecr_lifecycle_policy" "agent" {
  repository = aws_ecr_repository.agent.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the newest ${var.retained_images} images"
      selection = {
        tagStatus   = "any"
        countType   = "imageCountMoreThan"
        countNumber = var.retained_images
      }
      action = { type = "expire" }
    }]
  })
}

module "agent" {
  source = "../modules/agent"

  image_uri        = var.image_digest == null ? null : "${aws_ecr_repository.agent.repository_url}@${var.image_digest}"
  repository_arn   = aws_ecr_repository.agent.arn
  jmap_session_url = var.jmap_session_url
  oauth_issuer     = var.oauth_issuer
  oauth_client_id  = var.oauth_client_id
  oauth_scopes     = var.oauth_scopes
  ask_model        = var.ask_model
  live_version     = var.live_version
}

# Agent-specific spend (docs/specs/agent.md#deployment-and-operations). Tag
# filtering needs the Component cost allocation tag activated once in Billing.
resource "aws_budgets_budget" "agent" {
  count = var.budget == null ? 0 : 1

  name         = "${var.project}-agent"
  budget_type  = "COST"
  limit_amount = tostring(var.budget.monthly_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  cost_filter {
    name   = "TagKeyValue"
    values = ["user:Component$agent"]
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = [var.budget.email]
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_email_addresses = [var.budget.email]
  }
}
