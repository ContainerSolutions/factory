# The dispatcher's state (P3): private, TLS-only, versioned. Only the instance role writes it
# (iam.tf); the bucket policy only enforces TLS.
locals {
  state_prefix = "factory"
  state_url    = "s3://${aws_s3_bucket.state.bucket}/${local.state_prefix}"
}

resource "aws_s3_bucket" "state" {
  bucket = "factory-state-${var.aws_account_id}"
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket = aws_s3_bucket.state.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# No ACLs: the account owns every object.
resource "aws_s3_bucket_ownership_controls" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# The guard against an accidental delete. The store never overwrites, so a version is kept
# only for what was deleted.
resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id

  versioning_configuration {
    status = "Enabled"
  }
}

# Expire what deletes leave behind. There is no expiration of current objects: the
# dispatcher deletes its own log entries, under its retention (P3).
resource "aws_s3_bucket_lifecycle_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  # Versioning must be on before noncurrent-version rules apply.
  depends_on = [aws_s3_bucket_versioning.state]

  rule {
    id     = "clean-up-deletes"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = 7
    }

    expiration {
      expired_object_delete_marker = true
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
}

resource "aws_s3_bucket_policy" "state" {
  bucket = aws_s3_bucket.state.id

  # A public-access block that is still being applied rejects the policy call.
  depends_on = [aws_s3_bucket_public_access_block.state]

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "DenyInsecureTransport"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:*"
        Resource = [
          "arn:aws:s3:::${aws_s3_bucket.state.bucket}",
          "arn:aws:s3:::${aws_s3_bucket.state.bucket}/*",
        ]
        Condition = {
          Bool = {
            "aws:SecureTransport" = "false"
          }
        }
      },
    ]
  })
}
