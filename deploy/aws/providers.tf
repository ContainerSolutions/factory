# The region, also in the ARNs the policies name and in the boot script.
locals {
  region = "eu-west-2"
}

provider "aws" {
  region = local.region

  # The provider refuses to act on any other account.
  allowed_account_ids = [var.aws_account_id]

  default_tags {
    tags = {
      app        = "factory"
      env        = "prod"
      managed-by = "opentofu"
    }
  }
}
