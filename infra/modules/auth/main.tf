# One Cognito user pool and one user (docs/specs/security.md#authentication-and-authorization):
# admin-created only, password sign-in without MFA (accepted risk), OAuth bearer tokens as the only API
# credential. App clients are public (no secret) and use code + PKCE, except
# the optional mail agent's one confidential client.

data "aws_region" "current" {}

resource "aws_cognito_user_pool" "this" {
  name                = var.name
  user_pool_tier      = "ESSENTIALS"
  deletion_protection = "ACTIVE"
  mfa_configuration   = "OFF"

  # Plain usernames: no email or phone attribute is stored, so no real
  # address lives in the pool. Recovery is by the owner's AWS admin access,
  # outside this mailbox.
  admin_create_user_config {
    allow_admin_create_user_only = true
  }

  account_recovery_setting {
    recovery_mechanism {
      name     = "admin_only"
      priority = 1
    }
  }

  # 8 characters and no composition rules, as chosen by the owner. NIST SP
  # 800-63B asks for 15 when a password is the only factor; see the accepted
  # risk in docs/specs/security.md#authentication-and-authorization.
  password_policy {
    minimum_length                   = 8
    require_lowercase                = false
    require_uppercase                = false
    require_numbers                  = false
    require_symbols                  = false
    temporary_password_validity_days = 3
  }

  sign_in_policy {
    allowed_first_auth_factors = ["PASSWORD"]
  }

  username_configuration {
    case_sensitive = false
  }

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_cognito_resource_server" "s3mail" {
  user_pool_id = aws_cognito_user_pool.this.id
  identifier   = "s3mail"
  name         = "s3mail JMAP API"

  scope {
    scope_name        = "mail.read"
    scope_description = "Read mail, changes, blobs and push"
  }

  scope {
    scope_name        = "mail.write"
    scope_description = "Mutate Email, Mailbox and Identity objects; upload and import"
  }

  scope {
    scope_name        = "mail.submit"
    scope_description = "Create, cancel and destroy submissions"
  }
}

# Terminal client for mail-auth and aerc. Refresh uses
# GetTokensFromRefreshToken because rotation is on, so
# ALLOW_REFRESH_TOKEN_AUTH is deliberately absent.
resource "aws_cognito_user_pool_client" "terminal" {
  name         = "terminal"
  user_pool_id = aws_cognito_user_pool.this.id

  generate_secret                      = false
  allowed_oauth_flows_user_pool_client = true
  allowed_oauth_flows                  = ["code"]
  allowed_oauth_scopes                 = aws_cognito_resource_server.s3mail.scope_identifiers
  callback_urls                        = var.terminal_callback_urls
  supported_identity_providers         = ["COGNITO"]
  explicit_auth_flows                  = ["ALLOW_USER_SRP_AUTH"]

  access_token_validity  = 5
  id_token_validity      = 5
  refresh_token_validity = 30
  auth_session_validity  = 3

  token_validity_units {
    access_token  = "minutes"
    id_token      = "minutes"
    refresh_token = "days"
  }

  enable_token_revocation       = true
  prevent_user_existence_errors = "ENABLED"

  refresh_token_rotation {
    feature                    = "ENABLED"
    retry_grace_period_seconds = 10
  }
}

# Browser webmail, registered only when its callbacks are set
# (docs/specs/clients.md#browser-hosting-and-client-safety). Its server
# exchanges the code and keeps the rotated refresh token in an httpOnly
# cookie; only the s3mail scopes are granted, so no ID token is issued.
resource "aws_cognito_user_pool_client" "webmail" {
  count = length(var.webmail_callback_urls) > 0 ? 1 : 0

  name         = "webmail"
  user_pool_id = aws_cognito_user_pool.this.id

  generate_secret                      = false
  allowed_oauth_flows_user_pool_client = true
  allowed_oauth_flows                  = ["code"]
  allowed_oauth_scopes                 = aws_cognito_resource_server.s3mail.scope_identifiers
  callback_urls                        = var.webmail_callback_urls
  supported_identity_providers         = ["COGNITO"]
  explicit_auth_flows                  = ["ALLOW_USER_SRP_AUTH"]

  access_token_validity  = 5
  id_token_validity      = 5
  refresh_token_validity = 30
  auth_session_validity  = 3

  token_validity_units {
    access_token  = "minutes"
    id_token      = "minutes"
    refresh_token = "days"
  }

  enable_token_revocation       = true
  prevent_user_existence_errors = "ENABLED"

  refresh_token_rotation {
    feature                    = "ENABLED"
    retry_grace_period_seconds = 10
  }
}

# The mail agent's client (docs/specs/security.md#client-registration-and-discovery),
# registered only when its token-vault callback is set. Confidential because
# the AgentCore Identity token vault holds its tokens server-side; the code
# grant still needs the owner's interactive consent, and client_credentials
# is never allowed. The owner replaces the generated secret out of band, so
# no live secret stays in state. Refresh is at the token endpoint by the
# vault, which keeps one refresh token: rotation stays off for this client.
resource "aws_cognito_user_pool_client" "agent" {
  count = length(var.agent_callback_urls) > 0 ? 1 : 0

  name         = "agent"
  user_pool_id = aws_cognito_user_pool.this.id

  generate_secret                      = true
  allowed_oauth_flows_user_pool_client = true
  allowed_oauth_flows                  = ["code"]
  allowed_oauth_scopes                 = [for s in var.agent_scopes : "${aws_cognito_resource_server.s3mail.identifier}/${s}"]
  callback_urls                        = var.agent_callback_urls
  supported_identity_providers         = ["COGNITO"]
  explicit_auth_flows                  = ["ALLOW_REFRESH_TOKEN_AUTH"]

  access_token_validity  = 5
  id_token_validity      = 5
  refresh_token_validity = 30
  auth_session_validity  = 3

  token_validity_units {
    access_token  = "minutes"
    id_token      = "minutes"
    refresh_token = "days"
  }

  enable_token_revocation       = true
  prevent_user_existence_errors = "ENABLED"

  # The owner-managed secret is added and the generated one deleted outside
  # Terraform; a later plan must not try to restore it.
  lifecycle {
    ignore_changes = [generate_secret]
  }
}

resource "aws_cognito_user_pool_domain" "this" {
  user_pool_id          = aws_cognito_user_pool.this.id
  domain                = var.domain_prefix
  managed_login_version = 2
}

# Managed login v2 is unavailable to a client until it has a branding style.
resource "aws_cognito_managed_login_branding" "terminal" {
  user_pool_id                = aws_cognito_user_pool.this.id
  client_id                   = aws_cognito_user_pool_client.terminal.id
  use_cognito_provided_values = true
}

resource "aws_cognito_managed_login_branding" "webmail" {
  count = length(aws_cognito_user_pool_client.webmail)

  user_pool_id                = aws_cognito_user_pool.this.id
  client_id                   = aws_cognito_user_pool_client.webmail[0].id
  use_cognito_provided_values = true
}

resource "aws_cognito_managed_login_branding" "agent" {
  count = length(aws_cognito_user_pool_client.agent)

  user_pool_id                = aws_cognito_user_pool.this.id
  client_id                   = aws_cognito_user_pool_client.agent[0].id
  use_cognito_provided_values = true
}
