variable "github_repo" {
  description = "owner/name of the repository allowed to deploy."
  type        = string
}

variable "github_repo_immutable" {
  description = "owner@owner_id/name@repo_id: the repository in GitHub's immutable OIDC subject, for a repository that sends it. Empty trusts the name form only."
  type        = string
  default     = ""
}

variable "role_name" {
  type    = string
  default = "opendesk-github-deploy"
}

variable "create_oidc_provider" {
  description = "false when the account already has the GitHub OIDC provider."
  type        = bool
  default     = true
}

variable "existing_oidc_provider_arn" {
  type    = string
  default = null
}

variable "cluster_name" {
  type = string
}

variable "service_names" {
  type = list(string)
}

variable "migrate_family" {
  type = string
}

variable "migrate_log_group_arn" {
  type = string
}

variable "ecr_repository_arns" {
  type = list(string)
}

variable "pass_role_arns" {
  description = "Task and execution roles the new task definition revisions reference."
  type        = list(string)
}

variable "terraform_plan_role_name" {
  type    = string
  default = "opendesk-github-terraform-plan"
}

variable "terraform_apply_role_name" {
  type    = string
  default = "opendesk-github-terraform-apply"
}

variable "terraform_state_bucket" {
  description = "The state bucket, which also holds the settings (<key>.tfvars)."
  type        = string
}

variable "terraform_state_kms_key_arn" {
  description = "The KMS key the state bucket is encrypted with, when it has one of its own (recommended; deploy/terraform/README.md, \"State\"). Empty for SSE-S3."
  type        = string
  default     = ""
}

variable "terraform_state_keys" {
  description = "Objects in the state bucket the plan role reads: the state and the settings."
  type        = list(string)
  default     = ["opendesk/production.tfstate", "opendesk/production.tfvars"]
}

variable "secret_prefix" {
  description = "Secrets Manager name prefix of the project's secrets, e.g. opendesk/production."
  type        = string
}

variable "terraform_secret_arns" {
  description = "The secrets whose values Terraform itself writes (and a refresh reads back). The plan role reads no other secret value: operator-supplied ones (API keys, OAuth client secrets) stay out of its reach."
  type        = list(string)
}

variable "kms_key_arn" {
  description = "The key the secrets are encrypted with."
  type        = string
}
