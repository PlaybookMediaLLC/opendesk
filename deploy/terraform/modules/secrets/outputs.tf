output "arns" {
  description = "Secret key => ARN (never the values)."
  value       = { for k, s in aws_secretsmanager_secret.this : k => s.arn }
}

output "managed_value_arns" {
  description = "ARNs of the secrets whose values Terraform writes (generated and database), the only ones a plan's refresh reads."
  value = concat(
    [for k in local.generated_keys : aws_secretsmanager_secret.this[k].arn],
    [for k in local.database_keys : aws_secretsmanager_secret.this[k].arn],
  )
}

output "generated_keys" {
  value = local.generated_keys
}

output "manual_keys" {
  value = local.manual
}
