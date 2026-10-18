# Offline checks of the app clients (docs/specs/security.md#acceptance-checks).

mock_provider "aws" {
  mock_data "aws_region" {
    defaults = { region = "us-east-1" }
  }

  mock_resource "aws_cognito_resource_server" {
    defaults = { identifier = "s3mail" }
  }
}

variables {
  domain_prefix          = "s3mail-test"
  terminal_callback_urls = ["http://localhost:8400/callback"]
}

run "no_agent_client_by_default" {
  command = plan

  assert {
    condition     = length(aws_cognito_user_pool_client.agent) == 0
    error_message = "the agent client exists only once its callback is set"
  }
}

run "agent_client_is_code_only_with_one_callback_and_read_scope" {
  command = apply

  variables {
    agent_callback_urls = ["https://bedrock-agentcore.us-east-1.amazonaws.com/identities/oauth2/callback/00000000-0000-4000-8000-000000000000"]
  }

  assert {
    condition = (
      aws_cognito_user_pool_client.agent[0].allowed_oauth_flows == toset(["code"]) &&
      aws_cognito_user_pool_client.agent[0].generate_secret
    )
    error_message = "a confidential client with the authorization code grant only (never client_credentials)"
  }

  assert {
    condition     = aws_cognito_user_pool_client.agent[0].allowed_oauth_scopes == toset(["s3mail/mail.read"])
    error_message = "A0 grants mail.read only"
  }

  assert {
    condition     = length(aws_cognito_user_pool_client.agent[0].callback_urls) == 1
    error_message = "the token-vault callback is the only redirect URI"
  }

  assert {
    condition     = aws_cognito_user_pool_client.terminal.generate_secret == false
    error_message = "the terminal client stays public"
  }
}

run "agent_scopes_grow_only_by_name" {
  command = plan

  variables {
    agent_callback_urls = ["https://bedrock-agentcore.us-east-1.amazonaws.com/identities/oauth2/callback/x"]
    agent_scopes        = ["mail.read", "aws.cognito.signin.user.admin"]
  }

  expect_failures = [var.agent_scopes]
}

run "agent_callback_is_https" {
  command = plan

  variables {
    agent_callback_urls = ["http://bedrock-agentcore.example.com/callback"]
  }

  expect_failures = [var.agent_callback_urls]
}
