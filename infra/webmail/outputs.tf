output "repository_url" {
  description = "Push target for make webmail-image (WEBMAIL_REPO)."
  value       = aws_ecr_repository.webmail.repository_url
}

output "url" {
  description = "The webmail origin, once an image is deployed."
  value       = one(module.webmail[*].url)
}

output "function" {
  description = "Function name and the version behind its live alias."
  value       = one([for m in module.webmail : { name = m.function_name, version = m.version }])
}
