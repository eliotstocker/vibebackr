provider "aws" {
  region              = var.aws_region
  shared_config_files = ["${path.module}/.aws_tf_config"]
}

data "aws_caller_identity" "current" {}

# CloudFront requires ACM SSL certificates to reside in us-east-1
provider "aws" {
  alias               = "us_east_1"
  region              = "us-east-1"
  shared_config_files = ["${path.module}/.aws_tf_config"]
}

provider "neon" {
  api_key = var.neon_api_key
}

# Auto-generate a secure random JWT HMAC signing secret if one was not provided
resource "random_password" "jwt_secret" {
  length  = 48
  special = false
}
