# The kit's only variable: everything else is written in (P4 §The kit).
variable "aws_account_id" {
  description = "The 12-digit ID of the AWS account the kit deploys to."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.aws_account_id))
    error_message = "aws_account_id must be a 12-digit AWS account ID."
  }
}
