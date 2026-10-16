variable "zone_name" {
  description = "Domain the hosted zone serves, without a trailing dot (set in terraform.tfvars)."
  type        = string

  validation {
    condition     = can(regex("^([a-z0-9]([a-z0-9-]*[a-z0-9])?\\.)+[a-z]{2,63}$", var.zone_name))
    error_message = "zone_name must be a lowercase domain name without a trailing dot."
  }
}

variable "records" {
  description = <<-EOT
    Merged dns_records declarations from the enabled modules. name is relative
    to the zone ("" for the apex); values use Route 53 syntax (TXT values
    quoted, MX "<priority> <host>", SRV "<priority> <weight> <port> <host>").
  EOT
  type = list(object({
    name    = string
    type    = string
    ttl     = number
    values  = list(string)
    purpose = string
  }))
  default = []

  validation {
    condition     = alltrue([for r in var.records : contains(["A", "AAAA", "CAA", "CNAME", "MX", "SRV", "TXT"], r.type)])
    error_message = "Only portable record types are allowed: A, AAAA, CAA, CNAME, MX, SRV, TXT."
  }

  validation {
    condition     = alltrue([for r in var.records : !(r.type == "CNAME" && r.name == "")])
    error_message = "A CNAME cannot be at the zone apex."
  }

  validation {
    condition     = alltrue([for r in var.records : r.type != "CNAME" || length(r.values) == 1])
    error_message = "A CNAME has exactly one value."
  }

  validation {
    condition     = alltrue([for r in var.records : r.name == "" || can(regex("^[a-z0-9_]([a-z0-9_-]*[a-z0-9])?(\\.[a-z0-9_]([a-z0-9_-]*[a-z0-9])?)*$", r.name))])
    error_message = "Record names are lowercase, relative to the zone, without a trailing dot."
  }

  validation {
    condition     = alltrue([for r in var.records : length(r.values) > 0 && r.ttl >= 60 && trimspace(r.purpose) != ""])
    error_message = "Every record needs at least one value, a TTL of at least 60 s and a purpose."
  }

  validation {
    condition     = length(distinct([for r in var.records : "${r.name} ${r.type}"])) == length(var.records)
    error_message = "Declare each name and type once, with all its values together."
  }

  validation {
    # A CNAME cannot share its name with any other record.
    condition = alltrue([
      for r in var.records : r.type != "CNAME" || length([for o in var.records : o if o.name == r.name]) == 1
    ])
    error_message = "A CNAME name cannot have other records."
  }
}
