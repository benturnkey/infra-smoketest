terraform {
  required_version = ">= 1.7.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

locals {
  # Consume the same values as the controller and ServiceAccount manifests.
  kustomization = yamldecode(file("${path.module}/../../../../aws-pod-identity-webhook/kustomization.yaml"))
  aws_config = one([
    for config in local.kustomization.configMapGenerator : config
    if config.name == "infra-smoketest-aws-config"
  ])
  aws_settings = {
    for literal in local.aws_config.literals :
    split("=", literal)[0] => join("=", slice(split("=", literal), 1, length(split("=", literal))))
  }
}

provider "aws" {
  region              = var.region
  allowed_account_ids = [local.aws_settings.AWS_ACCOUNT_ID]
}

module "identity" {
  source = "../.."

  aws_account_id    = local.aws_settings.AWS_ACCOUNT_ID
  role_name         = local.aws_settings.AWS_IDENTITY_ROLE_NAME
  oidc_provider_url = var.oidc_provider_url
  tags              = var.tags
}

output "role_arn" {
  description = "Role ARN to compare with the rendered identity ServiceAccount annotation."
  value       = module.identity.role_arn
}
