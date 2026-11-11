# The dispatcher: one t4g.micro running factory serve and Caddy as two containers (P4 §The
# instance). Its secrets are SSM parameters under /factory/prod/env/, put by hand (README);
# OpenTofu never sees them.
locals {
  domain = "factory.container-solutions.com"

  # The boot script. It holds no secret: factory-run reads them from SSM when the unit starts.
  user_data = templatefile("${path.module}/user_data.sh.tftpl", {
    region    = local.region
    log_group = aws_cloudwatch_log_group.factory.name
    domain    = local.domain
    state_url = local.state_url
  })
}

# The latest Amazon Linux 2023 arm64 image. The instance ignores later changes (below): a new
# image is a deliberate `tofu apply -replace=aws_instance.factory`.
data "aws_ssm_parameter" "al2023" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"
}

resource "aws_security_group" "factory" {
  name        = "factory"
  description = "factory: HTTP and HTTPS in (Caddy), all out. No SSH: Session Manager."
  vpc_id      = aws_vpc.factory.id

  tags = {
    Name = "factory"
  }
}

# 80 for Let's Encrypt's HTTP-01 challenge and the redirect, 443 for everything else.
resource "aws_vpc_security_group_ingress_rule" "factory" {
  for_each = {
    http  = 80
    https = 443
  }

  security_group_id = aws_security_group.factory.id
  description       = each.key
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = each.value
  to_port           = each.value
}

resource "aws_vpc_security_group_egress_rule" "factory" {
  security_group_id = aws_security_group.factory.id
  description       = "all"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

# Both containers' logs (the awslogs driver), streams factory and caddy.
resource "aws_cloudwatch_log_group" "factory" {
  name              = "/factory/prod"
  retention_in_days = 30
}

# What the units read at start, so an upgrade is an edit here, an apply and a restart. Plain
# String parameters: nothing here is secret, and insecure_value keeps the digests visible in plans.
resource "aws_ssm_parameter" "image" {
  name = "/factory/prod/image"
  type = "String"
  # factory v0.3.1
  insecure_value = "ghcr.io/containersolutions/factory@sha256:998782592a065458f8bbce5602a8ddf38c26bf3c9db6bd676056612f5b62a0df"
}

resource "aws_ssm_parameter" "caddy_image" {
  name = "/factory/prod/caddy-image"
  type = "String"
  # caddy 2.11.9
  insecure_value = "docker.io/library/caddy@sha256:0c2243efcf437ab7f7a978f1027fcf0d4f9be8a75df27b1b3fbc5dbe0ffa8cf0"
}

# serve's flags beyond those factory-run sets. It word-splits them: no value may hold a space.
resource "aws_ssm_parameter" "args" {
  name           = "/factory/prod/args"
  type           = "String"
  insecure_value = "--catalog-repo https://github.com/compliance-framework/factory-catalog --catalog-app-id 5343156 --catalog-app-key-ref ccf-factory-app/private-key --retention 2160h"
}

resource "aws_instance" "factory" {
  ami                    = nonsensitive(data.aws_ssm_parameter.al2023.value)
  instance_type          = "t4g.micro"
  subnet_id              = aws_subnet.factory.id
  vpc_security_group_ids = [aws_security_group.factory.id]
  iam_instance_profile   = aws_iam_instance_profile.dispatcher.name

  # The Elastic IP is its public address.
  associate_public_ip_address = false

  # A busy spell slows it down rather than billing surplus credits.
  credit_specification {
    cpu_credits = "standard"
  }

  # IMDSv2 only, one hop: the containers use the host's network, nothing else reaches it.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  # Encrypted with the AWS-managed aws/ebs key.
  root_block_device {
    volume_type = "gp3"
    volume_size = 8
    encrypted   = true
  }

  user_data = local.user_data
  # A changed script replaces the instance: the state is in the bucket and the Elastic IP moves.
  user_data_replace_on_change = true

  # The units read these on their first start.
  depends_on = [
    aws_cloudwatch_log_group.factory,
    aws_iam_role_policy.dispatcher,
    aws_ssm_parameter.args,
    aws_ssm_parameter.caddy_image,
    aws_ssm_parameter.image,
  ]

  tags = {
    Name = "factory"
  }

  lifecycle {
    ignore_changes = [ami]
  }
}

resource "aws_eip" "factory" {
  domain   = "vpc"
  instance = aws_instance.factory.id

  # An Elastic IP in a VPC needs its internet gateway.
  depends_on = [aws_internet_gateway.factory]

  tags = {
    Name = "factory"
  }
}
