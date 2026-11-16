# Mocked: no credentials, never reaches AWS.
mock_provider "aws" {}

variables {
  aws_account_id = "111122223333"
}

run "subnet_is_public_in_eu_west_2a" {
  command = plan

  assert {
    condition     = aws_subnet.factory.availability_zone == "eu-west-2a"
    error_message = "the subnet must be in eu-west-2a"
  }

  assert {
    condition     = aws_subnet.factory.map_public_ip_on_launch == false
    error_message = "the instance gets the Elastic IP, not an auto-assigned public IP"
  }

  assert {
    condition     = aws_vpc.factory.enable_dns_support && aws_vpc.factory.enable_dns_hostnames
    error_message = "the VPC needs DNS support and hostnames"
  }
}

run "default_route_goes_to_the_internet_gateway" {
  command = plan

  assert {
    condition = length([
      for r in aws_route_table.factory.route : r
      if r.cidr_block == "0.0.0.0/0" && r.gateway_id == aws_internet_gateway.factory.id
    ]) == 1
    error_message = "the route table must send 0.0.0.0/0 to the internet gateway"
  }

  assert {
    condition     = length(aws_route_table.factory.route) == 1
    error_message = "the route table has only the default route (no NAT)"
  }
}

run "malformed_account_id_is_refused" {
  command = plan

  variables {
    aws_account_id = "123"
  }

  expect_failures = [var.aws_account_id]
}
