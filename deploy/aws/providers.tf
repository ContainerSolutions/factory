provider "aws" {
  region = "eu-west-2"

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
