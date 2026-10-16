output "invoke_url" {
  description = "Stage URL for spike-probe -url."
  value       = aws_api_gateway_stage.spike.invoke_url
}

output "resource_metadata_url" {
  description = "RFC 9728 metadata URL named in 401 challenges."
  value       = local.metadata_url
}
