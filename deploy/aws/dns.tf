# The factory's records in ccf.container-solutions.com. That public zone is made by hand and
# delegated from container-solutions.com by its owners; the kit only looks it up and adds the
# factory's records, and never creates or manages the zone.
locals {
  zone_name = "ccf.container-solutions.com"
}

data "aws_route53_zone" "ccf" {
  name         = local.zone_name
  private_zone = false
}

# The Elastic IP outlives the instance, so a replaced instance keeps the name.
resource "aws_route53_record" "factory_a" {
  zone_id = data.aws_route53_zone.ccf.zone_id
  name    = local.domain
  type    = "A"
  ttl     = 300
  records = [aws_eip.factory.public_ip]
}

# Only Let's Encrypt (Caddy's CA) may issue certificates for the name.
resource "aws_route53_record" "factory_caa" {
  zone_id = data.aws_route53_zone.ccf.zone_id
  name    = local.domain
  type    = "CAA"
  ttl     = 300
  records = ["0 issue \"letsencrypt.org\""]
}
