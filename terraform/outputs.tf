output "custom_domain_url" {
  description = "Public URL using custom domain"
  value       = "https://${var.domain_name}"
}

output "custom_mcp_endpoint" {
  description = "Custom domain streamable HTTP MCP endpoint (Server-Sent Events)"
  value       = "https://${var.domain_name}/mcp"
}

output "route53_nameservers" {
  description = "Route 53 Nameservers (set these at your domain registrar if domain is not managed in AWS)"
  value       = var.create_route53_zone ? aws_route53_zone.primary[0].name_servers : null
}

output "lambda_direct_url" {
  description = "Direct AWS Lambda Function URL"
  value       = aws_lambda_function_url.vibebackr_url.function_url
}

output "cloudfront_distribution_domain" {
  description = "CloudFront distribution domain name"
  value       = aws_cloudfront_distribution.cf.domain_name
}

output "neon_connection_string" {
  description = "Neon PostgreSQL connection URI routed through the connection pooler (sensitive)"
  value       = neon_project.vibebackr.connection_uri_pooler
  sensitive   = true
}

output "neon_project_id" {
  description = "Neon Project ID"
  value       = neon_project.vibebackr.id
}
