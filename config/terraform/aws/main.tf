data "aws_caller_identity" "current" {}

data "aws_iam_openid_connect_provider" "cluster" {
  url = var.oidc_provider_url
}

locals {
  # These match the controller's namespace and approved identity ServiceAccount.
  service_account_subject = "system:serviceaccount:infra-smoketest:infra-smoketest-aws"
  oidc_issuer             = trimprefix(data.aws_iam_openid_connect_provider.cluster.url, "https://")
}

resource "aws_iam_role" "identity" {
  name        = var.role_name
  path        = "/"
  description = "IRSA role for the infrastructure smoke-test identity probe"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = "sts:AssumeRoleWithWebIdentity"
      Principal = {
        Federated = data.aws_iam_openid_connect_provider.cluster.arn
      }
      Condition = {
        StringEquals = {
          "${local.oidc_issuer}:sub" = local.service_account_subject
          "${local.oidc_issuer}:aud" = "sts.amazonaws.com"
        }
      }
    }]
  })

  # GetCallerIdentity requires no attached permissions policy.
  tags = merge({ "app.kubernetes.io/name" = "infra-smoketest" }, var.tags)

  lifecycle {
    precondition {
      condition     = data.aws_caller_identity.current.account_id == var.aws_account_id
      error_message = "The AWS provider account must match the shared smoke-test AWS account."
    }
    precondition {
      condition     = split(":", data.aws_iam_openid_connect_provider.cluster.arn)[4] == var.aws_account_id
      error_message = "The IAM OIDC provider must belong to the shared smoke-test AWS account."
    }
    precondition {
      condition     = contains(data.aws_iam_openid_connect_provider.cluster.client_id_list, "sts.amazonaws.com")
      error_message = "The existing IAM OIDC provider must allow the sts.amazonaws.com audience."
    }
  }
}
