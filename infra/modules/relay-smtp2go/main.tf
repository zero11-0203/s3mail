# SMTP2GO relay adapter configuration (docs/specs/mail-flow.md#outbound-relay-interface):
# the smtp adapter's endpoint and the sender-domain CNAMEs (DKIM, return
# path and the link domain SMTP2GO requires; tracking stays disabled).
# The sender domain is added in the SMTP2GO dashboard, which shows the
# CNAME targets; they are copied into terraform.tfvars. The account and credentials are out of band.

locals {
  records = [for r in var.cnames : {
    name    = r.name
    type    = "CNAME"
    ttl     = var.dns_ttl
    values  = [r.target]
    purpose = "SMTP2GO sender domain: ${r.purpose}"
  }]
}
