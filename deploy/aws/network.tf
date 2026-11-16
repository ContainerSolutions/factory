# One public subnet and an internet gateway: the account's default VPC may be gone, and the
# instance needs a public IPv4 (the Elastic IP) for GitHub's webhooks. No NAT.
resource "aws_vpc" "factory" {
  cidr_block           = "10.42.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = {
    Name = "factory"
  }
}

resource "aws_subnet" "factory" {
  vpc_id            = aws_vpc.factory.id
  cidr_block        = "10.42.0.0/24"
  availability_zone = "eu-west-2a"

  # The instance gets the Elastic IP instead.
  map_public_ip_on_launch = false

  tags = {
    Name = "factory-public"
  }
}

resource "aws_internet_gateway" "factory" {
  vpc_id = aws_vpc.factory.id

  tags = {
    Name = "factory"
  }
}

resource "aws_route_table" "factory" {
  vpc_id = aws_vpc.factory.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.factory.id
  }

  tags = {
    Name = "factory-public"
  }
}

resource "aws_route_table_association" "factory" {
  subnet_id      = aws_subnet.factory.id
  route_table_id = aws_route_table.factory.id
}
