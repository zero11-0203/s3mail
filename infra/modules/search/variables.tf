variable "name" {
  description = "Prefix for the functions, roles, queues, bucket and log groups."
  type        = string
  default     = "s3mail"
}

variable "indexer_zip" {
  description = "Path to the search-indexer Lambda zip (make build)."
  type        = string
}

variable "search_zip" {
  description = "Path to the search Lambda zip (make build)."
  type        = string
}

variable "table_name" {
  type = string
}

variable "table_arn" {
  type = string
}

variable "account_id" {
  description = "Internal account id (module.mail_store.account_id)."
  type        = string
}

variable "account_partition" {
  description = "The account's partition key, for dynamodb:LeadingKeys."
  type        = string
}

variable "originals_bucket_arn" {
  description = "Permanent original-mail bucket; the indexer reads exact versions under every prefix."
  type        = string
}

variable "log_retention_days" {
  type    = number
  default = 30
}

variable "embedding_model" {
  description = "Embeddings model id (OpenRouter) for semantic search; null keeps it off. Mail text then leaves AWS (docs/specs/search.md#semantic-search)."
  type        = string
  default     = null
}
