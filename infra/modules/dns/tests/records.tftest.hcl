# Offline checks of the record contract (docs/operations/deployment.md#acceptance-checks).

mock_provider "aws" {}

variables {
  zone_name = "example.com"
  records = [
    { name = "", type = "MX", ttl = 300, values = ["10 inbound-smtp.us-east-1.amazonaws.com"], purpose = "SES receiving" },
    { name = "_dmarc", type = "TXT", ttl = 300, values = ["\"v=DMARC1; p=none\""], purpose = "DMARC" },
    { name = "_jmap._tcp", type = "SRV", ttl = 300, values = ["0 1 443 jmap.example.com"], purpose = "JMAP autodiscovery" },
    { name = "jmap", type = "CNAME", ttl = 300, values = ["d-abc123.execute-api.us-east-1.amazonaws.com"], purpose = "API custom domain" },
  ]
}

run "valid_records_become_record_sets" {
  command = plan

  assert {
    condition     = length(aws_route53_record.this) == 4
    error_message = "every declared record becomes one record set"
  }

  assert {
    condition     = aws_route53_record.this["@ MX"].name == "example.com"
    error_message = "an empty name is the zone apex"
  }

  assert {
    condition     = aws_route53_record.this["_jmap._tcp SRV"].name == "_jmap._tcp.example.com"
    error_message = "relative names are qualified with the zone"
  }
}

run "no_records_is_valid" {
  command = plan

  variables {
    records = []
  }

  assert {
    condition     = length(aws_route53_record.this) == 0
    error_message = "the zone can exist before any module declares records"
  }
}

run "rejects_apex_cname" {
  command = plan

  variables {
    records = [{ name = "", type = "CNAME", ttl = 300, values = ["other.example.net"], purpose = "bad" }]
  }

  expect_failures = [var.records]
}

run "rejects_alias_style_type" {
  command = plan

  variables {
    records = [{ name = "www", type = "NS", ttl = 300, values = ["ns.example.net"], purpose = "bad" }]
  }

  expect_failures = [var.records]
}

run "rejects_duplicate_name_and_type" {
  command = plan

  variables {
    records = [
      { name = "", type = "TXT", ttl = 300, values = ["\"a\""], purpose = "one" },
      { name = "", type = "TXT", ttl = 300, values = ["\"b\""], purpose = "two" },
    ]
  }

  expect_failures = [var.records]
}

run "rejects_cname_sharing_a_name" {
  command = plan

  variables {
    records = [
      { name = "jmap", type = "CNAME", ttl = 300, values = ["x.example.net"], purpose = "one" },
      { name = "jmap", type = "TXT", ttl = 300, values = ["\"x\""], purpose = "two" },
    ]
  }

  expect_failures = [var.records]
}

run "rejects_fqdn_names" {
  command = plan

  variables {
    records = [{ name = "www.example.com.", type = "A", ttl = 300, values = ["192.0.2.1"], purpose = "bad" }]
  }

  expect_failures = [var.records]
}
