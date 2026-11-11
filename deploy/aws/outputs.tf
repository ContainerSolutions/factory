# For the parent zone's NS record on `factory` (container-solutions.com).
output "name_servers" {
  description = "The hosted zone's name servers, for the NS record that delegates the name."
  value       = aws_route53_zone.factory.name_servers
}

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
