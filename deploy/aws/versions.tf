# The AWS kit for factory.container-solutions.com (design/proposals/P4-dispatcher-on-aws.md).
# Built and tested with OpenTofu; the HCL stays valid for Terraform >= 1.10 too.
terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }

  # Partial configuration: `tofu init -backend-config=backend.hcl` (see backend.hcl.example),
  # so the repo names no state bucket.
  backend "s3" {}
}
