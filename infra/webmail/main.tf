# Phase 4 webmail, a root configuration with its own state, applied
# separately from infra/main (docs/operations/deployment.md#webmail-deployment).
# infra/main only registers the webmail (Cognito app client, authorizer
# allowlist, CORS origin) and owns the hosted zone; this root never reads
# its state and the mail system never depends on this one.
#
# Two steps on first deployment: apply with image_digest unset to create the
# repository, push the image (make webmail-image), then apply with its digest.

data "aws_route53_zone" "this" {
  name = var.domain
}

resource "aws_ecr_repository" "webmail" {
  name                 = "${var.project}-webmail"
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }
}

# Keep recent images so earlier digests stay available for rollback.
resource "aws_ecr_lifecycle_policy" "webmail" {
  repository = aws_ecr_repository.webmail.name
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

data "aws_caller_identity" "current" {}

# Lambda pulls the image as itself; only the webmail function may.
resource "aws_ecr_repository_policy" "webmail" {
  repository = aws_ecr_repository.webmail.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "LambdaPull"
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"]
      Condition = {
        StringLike = { "aws:sourceArn" = "arn:aws:lambda:${var.region}:${data.aws_caller_identity.current.account_id}:function:${var.project}-webmail*" }
      }
    }]
  })
}

module "webmail" {
  source = "../modules/webmail"
  count  = var.image_digest == null ? 0 : 1

  name            = "${var.project}-webmail"
  image_uri       = "${aws_ecr_repository.webmail.repository_url}@${var.image_digest}"
  domain          = var.domain
  subdomain       = var.subdomain
  zone_id         = data.aws_route53_zone.this.zone_id
  jmap_server_url = var.jmap_server_url
  oauth_issuer    = var.oauth_issuer
  oauth_client_id = var.oauth_client_id
  oauth_scopes    = var.oauth_scopes
  concurrency     = var.concurrency

  depends_on = [aws_ecr_repository_policy.webmail]
}

# Webmail-specific spend (docs/operations/monitoring.md). Tag filtering
# needs the Component cost allocation tag activated once in Billing.
resource "aws_budgets_budget" "webmail" {
  count = var.budget == null ? 0 : 1

  name         = "${var.project}-webmail"
  budget_type  = "COST"
  limit_amount = tostring(var.budget.monthly_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  cost_filter {
    name   = "TagKeyValue"
    values = ["user:Component$webmail"]
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
