variable "aws_region" {
  description = "AWS region for Lambda and CloudWatch"
  type        = string
  default     = "eu-west-2"
}

variable "neon_api_key" {
  description = "Neon API key (or set NEON_API_KEY environment variable)"
  type        = string
  sensitive   = true
  default     = null
}

variable "neon_region_id" {
  description = "Neon region ID (e.g. aws-eu-west-2, aws-us-east-1, aws-us-west-2, aws-eu-central-1)"
  type        = string
  default     = "aws-eu-west-2"
}

variable "neon_org_id" {
  description = "Neon Organization ID"
  type        = string
  default     = "org-bitter-boat-47013246"
}

variable "app_name" {
  description = "Application and resource naming prefix"
  type        = string
  default     = "vibe-backr"
}

variable "domain_name" {
  description = "Custom domain name"
  type        = string
  default     = "vibebac.kr"
}

variable "create_route53_zone" {
  description = "Whether to create a new Route 53 hosted zone for domain_name (set false if zone already exists)"
  type        = bool
  default     = true
}

variable "jwt_secret" {
  description = "JWT HMAC signing secret (auto-generated if left empty)"
  type        = string
  sensitive   = true
  default     = ""
}

variable "gemini_api_key" {
  description = "Optional Google Gemini API key for gemini-embedding-001 vector search"
  type        = string
  sensitive   = true
  default     = ""
}

variable "embedder_url" {
  description = "Optional URL to an EmbeddingGemma 2 or Ollama instance"
  type        = string
  default     = ""
}

variable "google_client_id" {
  description = "Google OAuth 2.0 Client ID"
  type        = string
  default     = ""
}

variable "google_client_secret" {
  description = "Google OAuth 2.0 Client Secret"
  type        = string
  sensitive   = true
  default     = ""
}

variable "enable_billing" {
  description = "Enable the per-tenant subscription billing and voucher module"
  type        = bool
  default     = false
}

variable "gocardless_access_token" {
  description = "GoCardless API access token"
  type        = string
  sensitive   = true
  default     = ""
}

variable "gocardless_webhook_secret" {
  description = "GoCardless webhook endpoint secret for HMAC-SHA256 verification"
  type        = string
  sensitive   = true
  default     = ""
}

variable "gocardless_env" {
  description = "GoCardless environment (sandbox or live)"
  type        = string
  default     = "live"
}

variable "voucher_codes" {
  description = "Comma-separated complimentary voucher codes to seed on startup (format: CODE[:plan[:max_uses]])"
  type        = string
  sensitive   = true
  default     = ""
}

variable "enable_rate_limit" {
  description = "Enable the per-collection token-bucket rate limiting module"
  type        = bool
  default     = false
}

variable "rate_limit_starter_rpm" {
  description = "Starter plan requests per minute per collection"
  type        = number
  default     = 60
}

variable "rate_limit_pro_rpm" {
  description = "Pro plan requests per minute per collection"
  type        = number
  default     = 180
}

variable "enable_collection_limits" {
  description = "Enable collection record count and per-record byte limits"
  type        = bool
  default     = true
}

variable "max_collection_records" {
  description = "Maximum records per collection when enable_collection_limits is true"
  type        = number
  default     = 500
}

variable "max_record_bytes" {
  description = "Maximum byte size per record when enable_collection_limits is true"
  type        = number
  default     = 65536
}

