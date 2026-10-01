output "role_arn" {
  description = "IAM role ARN expected by the controller and identity ServiceAccount annotation."
  value       = aws_iam_role.identity.arn
}

output "role_name" {
  description = "Created IAM role name."
  value       = aws_iam_role.identity.name
}

output "aws_account_id" {
  description = "AWS account containing the smoke-test role."
  value       = data.aws_caller_identity.current.account_id
}
