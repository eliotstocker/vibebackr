# Route 53 Hosted Zone
resource "aws_route53_zone" "primary" {
  count   = var.create_route53_zone ? 1 : 0
  name    = var.domain_name
  comment = "Managed zone for ${var.domain_name}"
}

data "aws_route53_zone" "existing" {
  count = var.create_route53_zone ? 0 : 1
  name  = var.domain_name
}

locals {
  zone_id = var.create_route53_zone ? aws_route53_zone.primary[0].zone_id : data.aws_route53_zone.existing[0].zone_id
}

# ACM Certificate in us-east-1 for CloudFront
resource "aws_acm_certificate" "cert" {
  provider                  = aws.us_east_1
  domain_name               = var.domain_name
  subject_alternative_names = ["*.${var.domain_name}"]
  validation_method         = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

# DNS Validation Records in Route 53
resource "aws_route53_record" "cert_validation" {
  for_each = {
    for dvo in aws_acm_certificate.cert.domain_validation_options : dvo.domain_name => {
      name   = dvo.resource_record_name
      record = dvo.resource_record_value
      type   = dvo.resource_record_type
    }
  }

  allow_overwrite = true
  name            = each.value.name
  records         = [each.value.record]
  ttl             = 60
  type            = each.value.type
  zone_id         = local.zone_id
}

# Complete ACM Certificate Validation
resource "aws_acm_certificate_validation" "cert" {
  provider                = aws.us_east_1
  certificate_arn         = aws_acm_certificate.cert.arn
  validation_record_fqdns = [for record in aws_route53_record.cert_validation : record.fqdn]
}

# Managed CloudFront policies
data "aws_cloudfront_cache_policy" "caching_disabled" {
  name = "Managed-CachingDisabled"
}

data "aws_cloudfront_cache_policy" "caching_optimized" {
  name = "Managed-CachingOptimized"
}

data "aws_cloudfront_origin_request_policy" "all_viewer_except_host_header" {
  name = "Managed-AllViewerExceptHostHeader"
}

# CloudFront Function for clean URL rewrites and agent content-negotiation
resource "aws_cloudfront_function" "url_rewrite" {
  name    = "${var.app_name}-url-rewrite"
  runtime = "cloudfront-js-2.0"
  comment = "Rewrite /login, /guide, /relay, /llms, collection app subdomains, and route agent Accept headers to /llms.txt"
  publish = true
  code    = <<-EOF
    function handler(event) {
      var request = event.request;
      var uri = request.uri;
      var headers = request.headers;
      var host = headers.host && headers.host.value ? headers.host.value.toLowerCase().split(":")[0] : "";

      // Subdomain app routing: {slug}.{domain} -> /apps/{slug}/index.html
      var domainSuffix = ".${var.domain_name}";
      var reserved = { "www": 1, "api": 1, "mcp": 1, "auth": 1, "oauth": 1, "login": 1, "guide": 1, "client-guide": 1, "nerds": 1, "relay": 1, "llms": 1, "apps": 1, "health": 1, "admin": 1, "mail": 1, "static": 1, "assets": 1 };
      if (host.length > domainSuffix.length && host.endsWith(domainSuffix)) {
        var sub = host.slice(0, host.length - domainSuffix.length);
        if (!reserved[sub] && /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(sub)) {
          if (uri === "/" || uri === "/index.html") {
            request.uri = "/apps/" + sub + "/index.html";
            return request;
          }
        }
      }

      // Path-based app routing fallback: /apps/{slug} -> /apps/{slug}/index.html
      if (uri.indexOf("/apps/") === 0) {
        var cleanPath = uri.replace(/\/+$/, "");
        var parts = cleanPath.split("/");
        if (parts.length === 3 && /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/i.test(parts[2])) {
          var appSlug = parts[2].toLowerCase();
          if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(appSlug)) {
            appSlug = appSlug.replace(/-/g, "");
          }
          request.uri = "/apps/" + appSlug + "/index.html";
          return request;
        }
      }

      // Content negotiation: If client explicitly requests markdown or plain text on root, redirect to /llms.txt
      if ((uri === "/" || uri === "/index.html") && headers.accept && headers.accept.value) {
        var accept = headers.accept.value.toLowerCase();
        if (accept.indexOf("text/markdown") !== -1 || (accept.indexOf("text/plain") !== -1 && accept.indexOf("text/html") === -1)) {
          return {
            statusCode: 302,
            statusDescription: "Found",
            headers: {
              "location": { value: "/llms.txt" }
            }
          };
        }
      }

      if (uri === "/login" || uri === "/login/") {
        request.uri = "/login.html";
      } else if (uri === "/guide" || uri === "/guide/") {
        request.uri = "/guide.html";
      } else if (uri === "/client-guide" || uri === "/client-guide/") {
        request.uri = "/guide.html";
      } else if (uri === "/nerds" || uri === "/nerds/") {
        request.uri = "/nerds.html";
      } else if (uri === "/relay" || uri === "/relay/") {
        request.uri = "/relay.html";
      } else if (uri === "/llms" || uri === "/llms/") {
        request.uri = "/llms.txt";
      }
      return request;
    }
  EOF
}

# CloudFront Response Headers Policy for Agent Discovery
resource "aws_cloudfront_response_headers_policy" "agent_headers" {
  name    = "${var.app_name}-agent-headers"
  comment = "Agent discovery headers: Link, X-Agent-Docs, and X-Mcp-Endpoint"

  custom_headers_config {
    items {
      header   = "Link"
      value    = "<https://${var.domain_name}/llms.txt>; rel=\"help\"; type=\"text/plain\", <https://${var.domain_name}/llms-full.txt>; rel=\"help\"; type=\"text/plain\", <https://${var.domain_name}/mcp>; rel=\"service-desc\""
      override = true
    }
    items {
      header   = "X-Agent-Docs"
      value    = "https://${var.domain_name}/llms.txt"
      override = true
    }
    items {
      header   = "X-Mcp-Endpoint"
      value    = "https://${var.domain_name}/mcp"
      override = true
    }
    items {
      header   = "X-Robots-Tag"
      value    = "all, index, follow"
      override = true
    }
  }
}

# CloudFront Distribution mapping vibebac.kr to S3 (frontend) and Lambda Function URL (API/MCP)
resource "aws_cloudfront_distribution" "cf" {
  enabled             = true
  is_ipv6_enabled     = true
  wait_for_deployment = false
  comment             = "${var.app_name} distribution for ${var.domain_name}"
  aliases             = [var.domain_name, "*.${var.domain_name}"]
  default_root_object = "index.html"

  # S3 Origin for static frontend
  origin {
    domain_name              = aws_s3_bucket.frontend.bucket_regional_domain_name
    origin_id                = "S3Frontend"
    origin_access_control_id = aws_cloudfront_origin_access_control.frontend.id
  }

  # Lambda Function URL Origin for API & MCP runtime
  origin {
    domain_name = replace(replace(aws_lambda_function_url.vibebackr_url.function_url, "https://", ""), "/", "")
    origin_id   = "LambdaFunctionURL"

    custom_origin_config {
      http_port              = 80
      https_port             = 443
      origin_protocol_policy = "https-only"
      origin_ssl_protocols   = ["TLSv1.2"]
    }
  }

  # Default behavior -> S3 frontend (Holding page, login page, assets)
  default_cache_behavior {
    target_origin_id           = "S3Frontend"
    viewer_protocol_policy     = "redirect-to-https"
    allowed_methods            = ["GET", "HEAD"]
    cached_methods             = ["GET", "HEAD"]
    cache_policy_id            = data.aws_cloudfront_cache_policy.caching_optimized.id
    response_headers_policy_id = aws_cloudfront_response_headers_policy.agent_headers.id

    function_association {
      event_type   = "viewer-request"
      function_arn = aws_cloudfront_function.url_rewrite.arn
    }
  }

  # Ordered Behaviors: Dynamic API, OAuth, Auth, MCP routes -> Lambda Function URL
  ordered_cache_behavior {
    path_pattern           = "/.well-known/*"
    target_origin_id       = "LambdaFunctionURL"
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD"]
    cached_methods         = ["GET", "HEAD"]
    cache_policy_id        = data.aws_cloudfront_cache_policy.caching_disabled.id
    origin_request_policy_id = data.aws_cloudfront_origin_request_policy.all_viewer_except_host_header.id
  }

  ordered_cache_behavior {
    path_pattern           = "/api/*"
    target_origin_id       = "LambdaFunctionURL"
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT"]
    cached_methods         = ["GET", "HEAD"]
    cache_policy_id        = data.aws_cloudfront_cache_policy.caching_disabled.id
    origin_request_policy_id = data.aws_cloudfront_origin_request_policy.all_viewer_except_host_header.id
  }

  ordered_cache_behavior {
    path_pattern           = "/oauth/*"
    target_origin_id       = "LambdaFunctionURL"
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT"]
    cached_methods         = ["GET", "HEAD"]
    cache_policy_id        = data.aws_cloudfront_cache_policy.caching_disabled.id
    origin_request_policy_id = data.aws_cloudfront_origin_request_policy.all_viewer_except_host_header.id
  }

  ordered_cache_behavior {
    path_pattern           = "/auth/*"
    target_origin_id       = "LambdaFunctionURL"
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT"]
    cached_methods         = ["GET", "HEAD"]
    cache_policy_id        = data.aws_cloudfront_cache_policy.caching_disabled.id
    origin_request_policy_id = data.aws_cloudfront_origin_request_policy.all_viewer_except_host_header.id
  }

  ordered_cache_behavior {
    path_pattern           = "/mcp"
    target_origin_id       = "LambdaFunctionURL"
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT"]
    cached_methods         = ["GET", "HEAD"]
    cache_policy_id        = data.aws_cloudfront_cache_policy.caching_disabled.id
    origin_request_policy_id = data.aws_cloudfront_origin_request_policy.all_viewer_except_host_header.id
  }

  ordered_cache_behavior {
    path_pattern           = "/mcp/*"
    target_origin_id       = "LambdaFunctionURL"
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT"]
    cached_methods         = ["GET", "HEAD"]
    cache_policy_id        = data.aws_cloudfront_cache_policy.caching_disabled.id
    origin_request_policy_id = data.aws_cloudfront_origin_request_policy.all_viewer_except_host_header.id
  }

  ordered_cache_behavior {
    path_pattern           = "/health"
    target_origin_id       = "LambdaFunctionURL"
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD"]
    cached_methods         = ["GET", "HEAD"]
    cache_policy_id        = data.aws_cloudfront_cache_policy.caching_disabled.id
    origin_request_policy_id = data.aws_cloudfront_origin_request_policy.all_viewer_except_host_header.id
  }

  restrictions {
    geo_restriction {
      restriction_type = "none"
    }
  }

  # Map S3 403s (missing object) to 404 with 0 TTL so crawlers never treat 403 as domain-wide block
  custom_error_response {
    error_code            = 403
    response_code         = 404
    response_page_path    = "/index.html"
    error_caching_min_ttl = 0
  }

  viewer_certificate {
    acm_certificate_arn      = aws_acm_certificate_validation.cert.certificate_arn
    ssl_support_method       = "sni-only"
    minimum_protocol_version = "TLSv1.2_2021"
  }

  depends_on = [aws_acm_certificate_validation.cert]
}

# Route 53 Apex Record (A Alias to CloudFront)
resource "aws_route53_record" "apex_a" {
  zone_id = local.zone_id
  name    = var.domain_name
  type    = "A"

  alias {
    name                   = aws_cloudfront_distribution.cf.domain_name
    zone_id                = aws_cloudfront_distribution.cf.hosted_zone_id
    evaluate_target_health = false
  }
}

# Route 53 Apex IPv6 Record (AAAA Alias to CloudFront)
resource "aws_route53_record" "apex_aaaa" {
  zone_id = local.zone_id
  name    = var.domain_name
  type    = "AAAA"

  alias {
    name                   = aws_cloudfront_distribution.cf.domain_name
    zone_id                = aws_cloudfront_distribution.cf.hosted_zone_id
    evaluate_target_health = false
  }
}

# Route 53 Wildcard Record (*.vibebac.kr, e.g. api.vibebac.kr)
resource "aws_route53_record" "wildcard_a" {
  zone_id = local.zone_id
  name    = "*.${var.domain_name}"
  type    = "A"

  alias {
    name                   = aws_cloudfront_distribution.cf.domain_name
    zone_id                = aws_cloudfront_distribution.cf.hosted_zone_id
    evaluate_target_health = false
  }
}

resource "aws_route53_record" "wildcard_aaaa" {
  zone_id = local.zone_id
  name    = "*.${var.domain_name}"
  type    = "AAAA"

  alias {
    name                   = aws_cloudfront_distribution.cf.domain_name
    zone_id                = aws_cloudfront_distribution.cf.hosted_zone_id
    evaluate_target_health = false
  }
}
