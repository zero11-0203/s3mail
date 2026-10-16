output "zone_id" {
  value = aws_route53_zone.this.zone_id
}

output "name_servers" {
  description = "Set these four nameservers at the registrar, once, after the zone answers correctly."
  value       = aws_route53_zone.this.name_servers
}

output "records" {
  description = "Declared records by \"name type\", for post-deploy resolution checks."
  value = {
    for k, r in local.records : k => {
      fqdn    = aws_route53_record.this[k].fqdn
      type    = r.type
      values  = r.values
      purpose = r.purpose
    }
  }
}
