# Everything the instance may do, in one place (P4 §The instance role), and the read-only role
# for reading the old state bucket from a laptop. The state now lives on the instance's disk
# (state.tf); the bucket access stays for the copy that switches to it (README §9). ARNs are built
# from names known at plan time.
locals {
  state_bucket_arn = "arn:aws:s3:::${aws_s3_bucket.state.bucket}"
  log_group_arn    = "arn:aws:logs:${local.region}:${var.aws_account_id}:log-group:${aws_cloudwatch_log_group.factory.name}"
}

resource "aws_iam_role" "dispatcher" {
  name = "factory-dispatcher"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect    = "Allow"
        Principal = { Service = "ec2.amazonaws.com" }
        Action    = "sts:AssumeRole"
      },
    ]
  })
}

resource "aws_iam_instance_profile" "dispatcher" {
  name = "factory-dispatcher"
  role = aws_iam_role.dispatcher.name
}

resource "aws_iam_role_policy" "dispatcher" {
  name = "factory-dispatcher"
  role = aws_iam_role.dispatcher.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "StateObjects"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
        Resource = "${local.state_bucket_arn}/${local.state_prefix}/*"
      },
      {
        # On the whole bucket, with no s3:prefix condition: S3 answers 404 NoSuchKey for a
        # missing key only to a caller with ListBucket on the bucket (else 403), and the store
        # reads up to the first missing log entry on every sync. The bucket holds only the state.
        Sid      = "StateList"
        Effect   = "Allow"
        Action   = "s3:ListBucket"
        Resource = local.state_bucket_arn
      },
      {
        # The image, serve's args and the secrets under env/. The parameters use the
        # AWS-managed aws/ssm key, which SSM decrypts for the account's principals: no kms:Decrypt.
        Sid    = "Parameters"
        Effect = "Allow"
        Action = ["ssm:GetParameter", "ssm:GetParametersByPath"]
        Resource = [
          "arn:aws:ssm:${local.region}:${var.aws_account_id}:parameter/factory/prod",
          "arn:aws:ssm:${local.region}:${var.aws_account_id}:parameter/factory/prod/*",
        ]
      },
      {
        # Docker's awslogs driver.
        Sid      = "Logs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = [local.log_group_arn, "${local.log_group_arn}:*"]
      },
    ]
  })
}

# Session Manager and Run Command: the only way onto the box (no SSH).
resource "aws_iam_role_policy_attachment" "dispatcher_ssm" {
  role       = aws_iam_role.dispatcher.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

# `factory state ... --state-url` on the old state bucket, from a laptop. Any principal in the
# account that its own policy allows may assume it.
resource "aws_iam_role" "state_readonly" {
  name = "factory-state-readonly"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect    = "Allow"
        Principal = { AWS = "arn:aws:iam::${var.aws_account_id}:root" }
        Action    = "sts:AssumeRole"
      },
    ]
  })
}

resource "aws_iam_role_policy" "state_readonly" {
  name = "factory-state-readonly"
  role = aws_iam_role.state_readonly.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "StateObjects"
        Effect   = "Allow"
        Action   = "s3:GetObject"
        Resource = "${local.state_bucket_arn}/${local.state_prefix}/*"
      },
      {
        # Unconditional, as for the dispatcher: a missing key must be 404, not 403.
        Sid      = "StateList"
        Effect   = "Allow"
        Action   = "s3:ListBucket"
        Resource = local.state_bucket_arn
      },
    ]
  })
}
