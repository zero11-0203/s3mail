# The Route 53 hosted zone and every DNS record, built from the record
# declarations other modules expose (docs/operations/deployment.md#dns-and-region).
# Only portable record types are allowed, so the DNS host can change later by
# replacing this module alone.

resource "aws_route53_zone" "this" {
  name    = var.zone_name
  comment = "s3mail; every record is managed by Terraform"

  lifecycle {
    prevent_destroy = true
  }
}

locals {
  # One Route 53 record set per name and type; values of the same set must
  # be declared together.
  records = {
    for r in var.records : "${r.name == "" ? "@" : r.name} ${r.type}" => r
  }
}

resource "aws_route53_record" "this" {
  for_each = local.records

  zone_id = aws_route53_zone.this.zone_id
  name    = each.value.name == "" ? var.zone_name : "${each.value.name}.${var.zone_name}"
  type    = each.value.type
  ttl     = each.value.ttl
  records = each.value.values
}
