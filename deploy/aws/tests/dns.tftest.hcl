# Mocked: no credentials, never reaches AWS.
mock_provider "aws" {
  override_data {
    target = data.aws_ssm_parameter.al2023
    values = {
      value = "ami-0123456789abcdef0"
    }
  }

  override_resource {
    target = aws_eip.factory
    values = {
      public_ip = "203.0.113.10"
    }
  }

  override_resource {
    target = aws_instance.factory
    values = {
      id = "i-0123456789abcdef0"
    }
  }

  override_data {
    target = data.aws_route53_zone.ccf
    values = {
      zone_id = "Z0123456789ABCDEFGHIJ"
    }
  }
}

variables {
  aws_account_id = "111122223333"
}

run "records_in_the_ccf_zone_point_at_the_elastic_ip" {
  command = plan

  assert {
    condition     = data.aws_route53_zone.ccf.name == "ccf.container-solutions.com" && data.aws_route53_zone.ccf.private_zone == false
    error_message = "the kit looks up the public ccf.container-solutions.com zone"
  }

  assert {
    condition = alltrue([
      for r in [aws_route53_record.factory_a, aws_route53_record.factory_caa] :
      r.zone_id == "Z0123456789ABCDEFGHIJ" && r.name == "factory.ccf.container-solutions.com"
    ])
    error_message = "both records are factory.ccf.container-solutions.com, in the looked-up ccf.container-solutions.com zone"
  }

  assert {
    condition = (
      aws_route53_record.factory_a.type == "A" &&
      aws_route53_record.factory_a.ttl == 300 &&
      aws_route53_record.factory_a.records == toset(["203.0.113.10"])
    )
    error_message = "the A record points at the Elastic IP"
  }

  assert {
    condition = (
      aws_route53_record.factory_caa.type == "CAA" &&
      aws_route53_record.factory_caa.records == toset(["0 issue \"letsencrypt.org\""])
    )
    error_message = "the CAA record allows only letsencrypt.org"
  }
}

run "alarm_watches_the_instance" {
  command = plan

  assert {
    condition = (
      aws_cloudwatch_metric_alarm.status_check.namespace == "AWS/EC2" &&
      aws_cloudwatch_metric_alarm.status_check.metric_name == "StatusCheckFailed" &&
      aws_cloudwatch_metric_alarm.status_check.dimensions == tomap({ InstanceId = "i-0123456789abcdef0" })
    )
    error_message = "the alarm watches the instance's status checks"
  }

  assert {
    condition = (
      aws_cloudwatch_metric_alarm.status_check.statistic == "Maximum" &&
      aws_cloudwatch_metric_alarm.status_check.period == 60 &&
      aws_cloudwatch_metric_alarm.status_check.evaluation_periods == 2 &&
      aws_cloudwatch_metric_alarm.status_check.comparison_operator == "GreaterThanOrEqualToThreshold" &&
      aws_cloudwatch_metric_alarm.status_check.threshold == 1 &&
      aws_cloudwatch_metric_alarm.status_check.treat_missing_data == "missing"
    )
    error_message = "the alarm fires after two failed minutes"
  }

  assert {
    condition     = aws_cloudwatch_metric_alarm.status_check.alarm_actions == toset(["arn:aws:sns:eu-west-2:111122223333:factory-alerts"])
    error_message = "the alarm notifies the alerts topic"
  }

  assert {
    condition     = aws_sns_topic.alerts.name == "factory-alerts"
    error_message = "the topic is factory-alerts"
  }
}

run "outputs" {
  command = plan

  assert {
    condition     = output.state_url == "s3://factory-state-111122223333/factory"
    error_message = "state_url is the bucket's factory prefix"
  }

  assert {
    condition     = output.instance_id == "i-0123456789abcdef0"
    error_message = "instance_id is the dispatcher's instance"
  }

  assert {
    condition     = output.public_ip == "203.0.113.10"
    error_message = "public_ip is the Elastic IP"
  }

  assert {
    condition     = output.alerts_topic_arn == "arn:aws:sns:eu-west-2:111122223333:factory-alerts"
    error_message = "alerts_topic_arn is the topic"
  }
}
