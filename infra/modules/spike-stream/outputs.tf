output "invoke_url" {
  description = "Stage URL for spike-probe -url."
  value       = aws_api_gateway_stage.spike.invoke_url
}
