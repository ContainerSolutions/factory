# All the alerting: the instance's status checks, mailed through SNS. The email subscription is
# one `aws sns subscribe` (README), since OpenTofu can't confirm it.
resource "aws_sns_topic" "alerts" {
  name = "factory-alerts"
}

# Built from the name, like the other ARNs here, so it is known at plan time.
locals {
  alerts_topic_arn = "arn:aws:sns:${local.region}:${var.aws_account_id}:${aws_sns_topic.alerts.name}"
}

resource "aws_cloudwatch_metric_alarm" "status_check" {
  alarm_name          = "factory-status-check-failed"
  alarm_description   = "The factory dispatcher's instance failed its EC2 status checks."
  namespace           = "AWS/EC2"
  metric_name         = "StatusCheckFailed"
  dimensions          = { InstanceId = aws_instance.factory.id }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  comparison_operator = "GreaterThanOrEqualToThreshold"
  threshold           = 1
  treat_missing_data  = "missing"
  alarm_actions       = [local.alerts_topic_arn]
}
