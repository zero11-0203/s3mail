# Offline checks of the table contract (docs/operations/deployment.md#acceptance-checks).

mock_provider "aws" {}

run "table_has_no_backups_and_is_protected" {
  command = plan

  assert {
    condition     = one(aws_dynamodb_table.mail.ttl).attribute_name == "ttl" && one(aws_dynamodb_table.mail.ttl).enabled
    error_message = "disposable records expire through the table TTL"
  }

  assert {
    condition     = one(aws_dynamodb_table.mail.point_in_time_recovery).enabled == false
    error_message = "DynamoDB PITR stays disabled"
  }

  assert {
    condition     = aws_dynamodb_table.mail.deletion_protection_enabled
    error_message = "deletion protection is on"
  }

  assert {
    condition     = aws_dynamodb_table.mail.hash_key == "PK" && aws_dynamodb_table.mail.range_key == "SK" && length(aws_dynamodb_table.mail.global_secondary_index) == 0
    error_message = "one PK/SK table without GSIs"
  }

  assert {
    condition     = output.account_partition == "ACCOUNT#primary"
    error_message = "the partition key names the account"
  }

  assert {
    condition     = aws_dynamodb_table.mail.stream_enabled && aws_dynamodb_table.mail.stream_view_type == "KEYS_ONLY"
    error_message = "the dispatcher stream carries keys only, never mail content"
  }
}
