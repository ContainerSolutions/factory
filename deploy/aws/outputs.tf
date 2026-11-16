output "state_url" {
  description = "The dispatcher's state, for `factory state ... --state-url`."
  value       = local.state_url
}

output "instance_id" {
  description = "The dispatcher's instance, for `aws ssm start-session` and `send-command`."
  value       = aws_instance.factory.id
}

output "public_ip" {
  description = "The Elastic IP the name points at."
  value       = aws_eip.factory.public_ip
}

output "alerts_topic_arn" {
  description = "The SNS topic the status-check alarm notifies; subscribe to it."
  value       = local.alerts_topic_arn
}
