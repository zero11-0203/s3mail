# The mail table (docs/specs/storage-sync.md#dynamodb-storage-and-consistency-contract):
# one partition per account (PK = ACCOUNT#<id>) with typed sort keys, no
# GSI on a correctness path. Encrypted at rest with the AWS owned key.
# A KEYS_ONLY stream triggers the outbox dispatcher; it carries no mail
# content. Point-in-time recovery stays disabled: no backups in the current scope
# (docs/operations/recovery.md#backup-and-export).

resource "aws_dynamodb_table" "mail" {
  name         = "${var.name}-mail"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "PK"
  range_key    = "SK"

  deletion_protection_enabled = true

  stream_enabled   = true
  stream_view_type = "KEYS_ONLY"

  attribute {
    name = "PK"
    type = "S"
  }

  attribute {
    name = "SK"
    type = "S"
  }

  point_in_time_recovery {
    enabled = false
  }

  lifecycle {
    prevent_destroy = true
  }
}
