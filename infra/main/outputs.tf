output "spike_stream_invoke_url" {
  description = "Stage URL for spike-probe -url, when the spike is enabled."
  value       = one(module.spike_stream[*].invoke_url)
}
