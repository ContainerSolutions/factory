# Mocked: no credentials, never reaches AWS.
mock_provider "aws" {}

variables {
  aws_account_id = "111122223333"
}

run "bucket_is_private" {
  command = plan

  assert {
    condition     = aws_s3_bucket.state.bucket == "factory-state-111122223333"
    error_message = "the bucket name must embed the account ID"
  }

  assert {
    condition = alltrue([
      aws_s3_bucket_public_access_block.state.block_public_acls,
      aws_s3_bucket_public_access_block.state.block_public_policy,
      aws_s3_bucket_public_access_block.state.ignore_public_acls,
      aws_s3_bucket_public_access_block.state.restrict_public_buckets,
    ])
    error_message = "all four public access block settings must be on"
  }

  assert {
    condition     = one(aws_s3_bucket_ownership_controls.state.rule).object_ownership == "BucketOwnerEnforced"
    error_message = "object ownership must be BucketOwnerEnforced"
  }

  assert {
    condition = one(one(aws_s3_bucket_server_side_encryption_configuration.state.rule)
    .apply_server_side_encryption_by_default).sse_algorithm == "AES256"
    error_message = "default encryption must be SSE-S3 (AES256)"
  }

  assert {
    condition     = one(aws_s3_bucket_versioning.state.versioning_configuration).status == "Enabled"
    error_message = "versioning must be on"
  }

  assert {
    condition     = local.state_url == "s3://factory-state-111122223333/factory"
    error_message = "the state URL must be s3://<bucket>/factory"
  }
}

run "lifecycle_cleans_up_deletes_only" {
  command = plan

  assert {
    condition     = length(aws_s3_bucket_lifecycle_configuration.state.rule) == 1
    error_message = "one lifecycle rule"
  }

  assert {
    condition     = one(aws_s3_bucket_lifecycle_configuration.state.rule[0].noncurrent_version_expiration).noncurrent_days == 7
    error_message = "noncurrent versions expire after 7 days"
  }

  assert {
    condition     = one(aws_s3_bucket_lifecycle_configuration.state.rule[0].expiration).expired_object_delete_marker == true
    error_message = "expired delete markers are removed"
  }

  assert {
    condition = (
      # days is 0 (the provider's "unset"), date is null.
      coalesce(one(aws_s3_bucket_lifecycle_configuration.state.rule[0].expiration).days, 0) == 0 &&
      one(aws_s3_bucket_lifecycle_configuration.state.rule[0].expiration).date == null
    )
    error_message = "current objects must never expire (the dispatcher deletes its own)"
  }

  assert {
    condition     = one(aws_s3_bucket_lifecycle_configuration.state.rule[0].abort_incomplete_multipart_upload).days_after_initiation == 1
    error_message = "incomplete multipart uploads are aborted after a day"
  }
}

run "policy_denies_non_tls" {
  command = plan

  assert {
    condition = anytrue([
      for s in jsondecode(aws_s3_bucket_policy.state.policy).Statement :
      s.Effect == "Deny" && s.Principal == "*" && s.Action == "s3:*" &&
      s.Condition.Bool["aws:SecureTransport"] == "false" &&
      toset(s.Resource) == toset([
        "arn:aws:s3:::factory-state-111122223333",
        "arn:aws:s3:::factory-state-111122223333/*",
      ])
    ])
    error_message = "the bucket policy must deny s3:* without TLS, on the bucket and its objects"
  }

  assert {
    condition = alltrue([
      for s in jsondecode(aws_s3_bucket_policy.state.policy).Statement : s.Effect == "Deny"
    ])
    error_message = "the bucket policy grants nothing (the instance role's access is IAM)"
  }
}
