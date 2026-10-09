# S3 Bucket for Static Frontend Assets (Holding page, Login page, CSS, Assets)
resource "aws_s3_bucket" "frontend" {
  bucket        = "${var.app_name}-frontend-${var.aws_region}-${data.aws_caller_identity.current.account_id}"
  force_destroy = true
}

# Block all public direct access to S3 bucket (only accessible via CloudFront OAC)
resource "aws_s3_bucket_public_access_block" "frontend" {
  bucket                  = aws_s3_bucket.frontend.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# CloudFront Origin Access Control (OAC)
resource "aws_cloudfront_origin_access_control" "frontend" {
  name                              = "${var.app_name}-frontend-oac"
  description                       = "OAC for ${var.app_name} frontend S3 bucket"
  origin_access_control_origin_type = "s3"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
}

# S3 Bucket Policy allowing CloudFront distribution to read objects
resource "aws_s3_bucket_policy" "frontend" {
  bucket = aws_s3_bucket.frontend.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "AllowCloudFrontServicePrincipalReadOnly"
        Effect    = "Allow"
        Principal = {
          Service = "cloudfront.amazonaws.com"
        }
        Action   = "s3:GetObject"
        Resource = "${aws_s3_bucket.frontend.arn}/*"
        Condition = {
          StringEquals = {
            "AWS:SourceArn" = aws_cloudfront_distribution.cf.arn
          }
        }
      }
    ]
  })
}

locals {
  mime_types = {
    "html" = "text/html"
    "css"  = "text/css"
    "js"   = "application/javascript"
    "ts"   = "application/typescript"
    "json" = "application/json"
    "svg"  = "image/svg+xml"
    "png"  = "image/png"
    "jpg"  = "image/jpeg"
    "ico"  = "image/x-icon"
    "txt"  = "text/plain; charset=utf-8"
    "xml"  = "application/xml; charset=utf-8"
    "mcpb" = "application/octet-stream"
  }
}

# Automatically sync compiled web/dist/ directory files to S3 with content types
resource "aws_s3_object" "frontend_files" {
  for_each = toset([for f in fileset("${path.module}/../web/dist", "**/*") : f if !startswith(f, "apps/")])

  bucket        = aws_s3_bucket.frontend.id
  key           = each.value
  source        = "${path.module}/../web/dist/${each.value}"
  source_hash   = filemd5("${path.module}/../web/dist/${each.value}")
  content_type  = lookup(local.mime_types, split(".", each.value)[length(split(".", each.value)) - 1], "text/plain")
  cache_control = each.value == "robots.txt" ? "public, max-age=60, must-revalidate" : null
}
