# Compile the pure-Go binary for Linux ARM64 (Graviton)
resource "terraform_data" "build_binary" {
  triggers_replace = [
    sha256(join("", [for f in fileset("${path.module}/..", "{cmd,internal}/**/*.{go,sql}") : filesha256("${path.module}/../${f}")])),
    filesha256("${path.module}/../go.mod"),
    filesha256("${path.module}/../go.sum")
  ]

  provisioner "local-exec" {
    command = "mkdir -p ${path.module}/dist && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags=\"-s -w\" -o ${path.module}/dist/vibebackr ${path.module}/../cmd/vibebackr"
  }
}

# Zip the compiled binary for Lambda deployment
data "archive_file" "lambda_zip" {
  type        = "zip"
  source_file = "${path.module}/dist/vibebackr"
  output_path = "${path.module}/dist/lambda.zip"

  depends_on = [terraform_data.build_binary]
}

# IAM Role for Lambda execution
resource "aws_iam_role" "lambda_exec" {
  name = "${var.app_name}-lambda-exec"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Action = "sts:AssumeRole"
        Effect = "Allow"
        Principal = {
          Service = "lambda.amazonaws.com"
        }
      }
    ]
  })
}

resource "aws_iam_role_policy_attachment" "lambda_basic_execution" {
  role       = aws_iam_role.lambda_exec.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

# Allow Lambda to publish and delete single-file collection apps in S3
resource "aws_iam_role_policy" "lambda_s3_apps" {
  name = "${var.app_name}-lambda-s3-apps"
  role = aws_iam_role.lambda_exec.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "s3:PutObject",
          "s3:DeleteObject"
        ]
        Resource = "${aws_s3_bucket.frontend.arn}/apps/*"
      }
    ]
  })
}

# CloudWatch Log Group with 14-day retention
resource "aws_cloudwatch_log_group" "lambda_logs" {
  name              = "/aws/lambda/${var.app_name}"
  retention_in_days = 14
}

# Lambda function running on provided.al2023 with AWS Lambda Web Adapter
resource "aws_lambda_function" "vibebackr" {
  function_name    = var.app_name
  role             = aws_iam_role.lambda_exec.arn
  handler          = "vibebackr"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  filename         = data.archive_file.lambda_zip.output_path
  source_code_hash = data.archive_file.lambda_zip.output_base64sha256

  timeout     = 60
  memory_size = 512

  layers = [
    # AWS Lambda Web Adapter layer for ARM64 (public AWS Labs layer)
    "arn:aws:lambda:${var.aws_region}:753240598075:layer:LambdaAdapterLayerArm64:24"
  ]

  environment {
    variables = {
      # Adapter configuration
      AWS_LAMBDA_EXEC_WRAPPER = "/opt/bootstrap"
      PORT                    = "8080"
      AWS_LWA_INVOKE_MODE     = "response_stream"

      # VibeBackr configuration
      DB_DRIVER        = "postgres"
      DATABASE_URL     = neon_project.vibebackr.connection_uri_pooler
      JWT_SECRET        = var.jwt_secret != "" ? var.jwt_secret : random_password.jwt_secret.result
      JWT_TTL           = "15m"  # 15-minute short-lived access token
      REFRESH_TOKEN_TTL = "720h" # 30-day long-lived refresh token
      GEMINI_API_KEY    = var.gemini_api_key
      EMBEDDER_URL      = var.embedder_url
      CANONICAL_DOMAIN  = var.domain_name
      VIBEBACKR_URL     = "https://${var.domain_name}"
      S3_BUCKET         = aws_s3_bucket.frontend.id
      S3_REGION         = var.aws_region

      # Google SSO
      GOOGLE_CLIENT_ID     = var.google_client_id
      GOOGLE_CLIENT_SECRET = var.google_client_secret
      GOOGLE_REDIRECT_URL  = var.google_client_id != "" ? "https://${var.domain_name}/auth/callback/google" : ""

      # Billing & GoCardless Module
      ENABLE_BILLING            = var.enable_billing || var.gocardless_access_token != "" ? "true" : "false"
      GOCARDLESS_ACCESS_TOKEN   = var.gocardless_access_token
      GOCARDLESS_WEBHOOK_SECRET = var.gocardless_webhook_secret
      GOCARDLESS_ENV            = var.gocardless_env
      VOUCHER_CODES             = var.voucher_codes

      # Per-Collection Rate Limiting Module
      ENABLE_RATE_LIMIT      = var.enable_rate_limit ? "true" : "false"
      RATE_LIMIT_STARTER_RPM = tostring(var.rate_limit_starter_rpm)
      RATE_LIMIT_PRO_RPM     = tostring(var.rate_limit_pro_rpm)

      # Collection Size Limits (disabled by default)
      ENABLE_COLLECTION_LIMITS = var.enable_collection_limits ? "true" : "false"
      MAX_COLLECTION_RECORDS   = tostring(var.max_collection_records)
      MAX_RECORD_BYTES         = tostring(var.max_record_bytes)
    }
  }

  depends_on = [
    aws_cloudwatch_log_group.lambda_logs,
    aws_iam_role_policy_attachment.lambda_basic_execution
  ]
}

# Lambda Function URL configured for Server-Sent Events (SSE) / response streaming
resource "aws_lambda_function_url" "vibebackr_url" {
  function_name      = aws_lambda_function.vibebackr.function_name
  authorization_type = "NONE"
  invoke_mode        = "RESPONSE_STREAM"
}

# Allow public invocation of the Lambda Function URL
resource "aws_lambda_permission" "url_permission" {
  statement_id           = "FunctionURLAllowPublicAccess"
  action                 = "lambda:InvokeFunctionUrl"
  function_name          = aws_lambda_function.vibebackr.function_name
  principal              = "*"
  function_url_auth_type = "NONE"
}

resource "aws_lambda_permission" "url_invoke_permission" {
  statement_id  = "FunctionURLInvokeAllowPublicAccess"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.vibebackr.function_name
  principal     = "*"
}

