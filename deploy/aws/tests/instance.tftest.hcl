# Mocked: no credentials, never reaches AWS.
mock_provider "aws" {
  override_data {
    target = data.aws_ssm_parameter.al2023
    values = {
      value = "ami-0123456789abcdef0"
    }
  }
}

variables {
  aws_account_id = "111122223333"
}

run "instance_is_locked_down" {
  command = plan

  assert {
    condition     = aws_instance.factory.ami == "ami-0123456789abcdef0"
    error_message = "the AMI comes from the AL2023 SSM parameter"
  }

  assert {
    condition     = aws_instance.factory.instance_type == "t4g.micro"
    error_message = "the instance is a t4g.micro"
  }

  assert {
    condition = (
      one(aws_instance.factory.metadata_options).http_tokens == "required" &&
      one(aws_instance.factory.metadata_options).http_put_response_hop_limit == 1 &&
      one(aws_instance.factory.metadata_options).http_endpoint == "enabled"
    )
    error_message = "IMDSv2 must be required, with a hop limit of 1"
  }

  assert {
    condition = (
      one(aws_instance.factory.root_block_device).encrypted == true &&
      one(aws_instance.factory.root_block_device).volume_type == "gp3" &&
      one(aws_instance.factory.root_block_device).volume_size == 8
    )
    error_message = "the root volume must be an encrypted 8 GiB gp3"
  }

  assert {
    condition     = one(aws_instance.factory.credit_specification).cpu_credits == "standard"
    error_message = "CPU credits must be standard (no surplus billing)"
  }

  assert {
    condition     = aws_instance.factory.user_data_replace_on_change == true
    error_message = "a changed boot script must replace the instance"
  }

  assert {
    condition     = aws_instance.factory.associate_public_ip_address == false
    error_message = "the instance's public address is the Elastic IP"
  }

  assert {
    condition     = aws_eip.factory.domain == "vpc"
    error_message = "the Elastic IP is a VPC address"
  }
}

run "only_http_and_https_are_open" {
  command = plan

  assert {
    condition     = toset([for r in aws_vpc_security_group_ingress_rule.factory : r.from_port]) == toset([80, 443])
    error_message = "the security group must open exactly 80 and 443"
  }

  assert {
    condition = alltrue([
      for r in aws_vpc_security_group_ingress_rule.factory :
      r.ip_protocol == "tcp" && r.from_port == r.to_port && r.cidr_ipv4 == "0.0.0.0/0"
    ])
    error_message = "each ingress rule is one TCP port from anywhere"
  }

  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.factory) == 2
    error_message = "no ingress rule beyond 80 and 443 (no SSH)"
  }

  assert {
    condition     = aws_vpc_security_group_egress_rule.factory.ip_protocol == "-1"
    error_message = "egress is open"
  }
}

run "dispatcher_role_reaches_only_its_prefix_parameters_and_logs" {
  command = plan

  assert {
    condition = toset(flatten([
      for s in jsondecode(aws_iam_role_policy.dispatcher.policy).Statement : s.Action
      ])) == toset([
      "s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket",
      "ssm:GetParameter", "ssm:GetParametersByPath",
      "logs:CreateLogStream", "logs:PutLogEvents",
    ])
    error_message = "the instance role grants exactly the actions P4 lists (no s3:*, no *)"
  }

  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.dispatcher.policy).Statement : s.Effect == "Allow"
    ])
    error_message = "the instance role's policy only allows"
  }

  assert {
    condition = toset(flatten([
      for s in jsondecode(aws_iam_role_policy.dispatcher.policy).Statement : s.Resource
      ])) == toset([
      "arn:aws:s3:::factory-state-111122223333/factory/*",
      "arn:aws:s3:::factory-state-111122223333",
      "arn:aws:ssm:eu-west-2:111122223333:parameter/factory/prod",
      "arn:aws:ssm:eu-west-2:111122223333:parameter/factory/prod/*",
      "arn:aws:logs:eu-west-2:111122223333:log-group:/factory/prod",
      "arn:aws:logs:eu-west-2:111122223333:log-group:/factory/prod:*",
    ])
    error_message = "the instance role reaches only the state prefix, /factory/prod parameters and the log group"
  }

  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.dispatcher.policy).Statement :
      s.Condition.StringLike["s3:prefix"] == ["factory/*"]
      if s.Action == "s3:ListBucket"
    ])
    error_message = "ListBucket is limited to the factory/ prefix"
  }

  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_role_policy.dispatcher.policy).Statement : s.Action == "s3:ListBucket"
    ])
    error_message = "the instance role may list the state prefix"
  }

  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role.dispatcher.assume_role_policy).Statement :
      s.Principal == { Service = "ec2.amazonaws.com" }
    ])
    error_message = "only EC2 assumes the instance role"
  }

  assert {
    condition     = aws_iam_role_policy_attachment.dispatcher_ssm.policy_arn == "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
    error_message = "Session Manager's managed policy is attached"
  }

  assert {
    condition     = aws_instance.factory.iam_instance_profile == aws_iam_instance_profile.dispatcher.name
    error_message = "the instance runs as the dispatcher role"
  }
}

run "readonly_role_cannot_write" {
  command = plan

  assert {
    condition = toset(flatten([
      for s in jsondecode(aws_iam_role_policy.state_readonly.policy).Statement : s.Action
    ])) == toset(["s3:GetObject", "s3:ListBucket"])
    error_message = "the read-only role may only get and list"
  }

  assert {
    condition = toset(flatten([
      for s in jsondecode(aws_iam_role_policy.state_readonly.policy).Statement : s.Resource
      ])) == toset([
      "arn:aws:s3:::factory-state-111122223333/factory/*",
      "arn:aws:s3:::factory-state-111122223333",
    ])
    error_message = "the read-only role reaches only the state prefix"
  }

  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role.state_readonly.assume_role_policy).Statement :
      s.Principal == { AWS = "arn:aws:iam::111122223333:root" }
    ])
    error_message = "only the account's own principals assume the read-only role"
  }
}

run "parameters_and_logs" {
  command = plan

  assert {
    condition     = aws_cloudwatch_log_group.factory.name == "/factory/prod" && aws_cloudwatch_log_group.factory.retention_in_days == 30
    error_message = "the log group is /factory/prod with 30 days' retention"
  }

  assert {
    condition = alltrue([
      for p in [aws_ssm_parameter.image, aws_ssm_parameter.caddy_image, aws_ssm_parameter.args] :
      p.type == "String" && startswith(p.name, "/factory/prod/") && !startswith(p.name, "/factory/prod/env/")
    ])
    error_message = "the kit's parameters are plain Strings outside env/ (factory-run exports env/ only)"
  }

  assert {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", aws_ssm_parameter.image.insecure_value)) && can(regex("@sha256:[0-9a-f]{64}$", aws_ssm_parameter.caddy_image.insecure_value))
    error_message = "both images are pinned by digest"
  }
}

run "user_data_runs_the_containers_and_holds_no_secret" {
  command = plan

  assert {
    condition = alltrue([
      for s in [
        "docker run --rm",
        "--state-url s3://factory-state-111122223333/factory",
        "--public-url https://factory.container-solutions.com",
        "--log-driver awslogs",
        "awslogs-group=/factory/prod",
        "-e AWS_REGION=eu-west-2",
        "--with-decryption",
        "factory.container-solutions.com {",
      ] : strcontains(aws_instance.factory.user_data, s)
    ])
    error_message = "the boot script must run both containers with the expected flags"
  }

  assert {
    condition = !anytrue([
      for s in ["PRIVATE KEY", "FACTORY_SESSION_KEY=", "set -x"] : strcontains(aws_instance.factory.user_data, s)
    ])
    error_message = "the boot script must hold no secret and never trace"
  }

  assert {
    condition     = aws_instance.factory.user_data == local.user_data
    error_message = "the instance runs the rendered boot script"
  }
}
