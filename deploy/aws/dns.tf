# The zone for factory.container-solutions.com. container-solutions.com stays where it is: its
# owners add an NS record for `factory` pointing at this zone's name servers (outputs.tf).
resource "aws_route53_zone" "factory" {
  name = local.domain
}

# The Elastic IP outlives the instance, so a replaced instance keeps the name.
resource "aws_route53_record" "apex_a" {
  zone_id = aws_route53_zone.factory.zone_id
  name    = local.domain
  type    = "A"
  ttl     = 300
  records = [aws_eip.factory.public_ip]
}

# Only Let's Encrypt (Caddy's CA) may issue certificates for the name.
resource "aws_route53_record" "apex_caa" {
  zone_id = aws_route53_zone.factory.zone_id
  name    = local.domain
  type    = "CAA"
  ttl     = 300
  records = ["0 issue \"letsencrypt.org\""]
}
