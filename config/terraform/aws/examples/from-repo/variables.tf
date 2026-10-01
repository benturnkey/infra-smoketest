variable "oidc_provider_url" {
  description = "HTTPS issuer URL of the cluster's existing IAM OIDC provider."
  type        = string
}

variable "region" {
  description = "AWS provider region; use the same region as the controller."
  type        = string
  default     = "us-east-1"
}

variable "tags" {
  description = "Additional tags for the IAM role."
  type        = map(string)
  default     = {}
}
