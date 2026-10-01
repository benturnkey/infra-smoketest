variable "aws_account_id" {
  description = "Existing AWS account that will own the role. Must match the configured AWS provider's account."
  type        = string
  nullable    = false

  validation {
    condition     = can(regex("^[0-9]{12}$", var.aws_account_id))
    error_message = "aws_account_id must contain exactly 12 digits."
  }
}

variable "role_name" {
  description = "IAM role name used by the controller and identity ServiceAccount, without an IAM path."
  type        = string
  nullable    = false

  validation {
    condition     = can(regex("^[A-Za-z0-9_+=,.@-]{1,64}$", var.role_name))
    error_message = "role_name must be a valid IAM role name of 1 to 64 characters, without a path."
  }
}

variable "oidc_provider_url" {
  description = "HTTPS issuer URL of the cluster's existing IAM OIDC provider (works with self-managed clusters and EKS)."
  type        = string
  nullable    = false

  validation {
    condition     = can(regex("^https://[^/?#*:]+(/[^?#*]*)?$", var.oidc_provider_url))
    error_message = "oidc_provider_url must be an HTTPS issuer URL without a query, fragment, port, or wildcard."
  }
}

variable "tags" {
  description = "Additional tags for the IAM role."
  type        = map(string)
  default     = {}
  nullable    = false
}
