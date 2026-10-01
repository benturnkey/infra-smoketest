mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
    }
  }

  mock_data "aws_iam_openid_connect_provider" {
    defaults = {
      arn            = "arn:aws:iam::123456789012:oidc-provider/issuer.example.com/cluster"
      client_id_list = ["sts.amazonaws.com"]
    }
  }
}

variables {
  aws_account_id    = "123456789012"
  role_name         = "infra-smoketest-aws"
  oidc_provider_url = "https://issuer.example.com/cluster"
}

run "trust_only_identity_service_account" {
  command = plan

  assert {
    condition = jsondecode(aws_iam_role.identity.assume_role_policy) == {
      Version = "2012-10-17"
      Statement = [{
        Effect = "Allow"
        Action = "sts:AssumeRoleWithWebIdentity"
        Principal = {
          Federated = "arn:aws:iam::123456789012:oidc-provider/issuer.example.com/cluster"
        }
        Condition = {
          StringEquals = {
            "issuer.example.com/cluster:sub" = "system:serviceaccount:infra-smoketest:infra-smoketest-aws"
            "issuer.example.com/cluster:aud" = "sts.amazonaws.com"
          }
        }
      }]
    }
    error_message = "Trust must allow only this cluster's identity ServiceAccount and the STS audience."
  }

  assert {
    condition     = aws_iam_role.identity.name == "infra-smoketest-aws" && aws_iam_role.identity.path == "/"
    error_message = "The role must match the ARN derived by the controller."
  }
}

run "reject_wrong_aws_account" {
  command = plan

  override_data {
    target = data.aws_caller_identity.current
    values = { account_id = "000000000000" }
  }

  expect_failures = [aws_iam_role.identity]
}

run "reject_oidc_provider_in_another_account" {
  command = plan

  override_data {
    target = data.aws_iam_openid_connect_provider.cluster
    values = {
      arn            = "arn:aws:iam::000000000000:oidc-provider/issuer.example.com/cluster"
      client_id_list = ["sts.amazonaws.com"]
    }
  }

  expect_failures = [aws_iam_role.identity]
}

run "reject_missing_sts_audience" {
  command = plan

  override_data {
    target = data.aws_iam_openid_connect_provider.cluster
    values = {
      arn            = "arn:aws:iam::123456789012:oidc-provider/issuer.example.com/cluster"
      client_id_list = ["other-audience"]
    }
  }

  expect_failures = [aws_iam_role.identity]
}
