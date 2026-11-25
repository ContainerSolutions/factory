output "state_url" {
  description = "The dispatcher's live state, on the instance's disk, for `factory state ... --state-url` on the box (README §9)."
  value       = local.state_url
}

output "bucket_state_url" {
  description = "The old state bucket, where the state lived before it moved to the instance's disk: the copy source for README §9, Switching from the bucket."
  value       = local.bucket_state_url
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
